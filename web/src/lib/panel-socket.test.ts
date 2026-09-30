import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { PanelSocket, type LiveSample } from '@/lib/panel-socket'
import { FakeWebSocket, openFake } from '@/test/websocket'

const serverA = '01924f9c-0000-7000-8000-0000000000aa'
const serverB = '01924f9c-0000-7000-8000-0000000000bb'

function sampleFor(serverID: string): LiveSample {
  return {
    server_id: serverID,
    sample: {
      ts: '2026-09-23T12:00:00Z',
      cpu_used_percent: 12,
      cpu_used_max_percent: 12,
      cpu_iowait_percent: 0,
      cpu_iowait_max_percent: 0,
      cpu_steal_percent: 0,
      cpu_cores: 4,
      mem_total_bytes: 1024,
      mem_used_bytes: 512,
      mem_used_max_bytes: 512,
      swap_total_bytes: 0,
      swap_used_bytes: 0,
      swap_used_max_bytes: 0,
      load1: 0.1,
      load5: 0.2,
      load15: 0.3,
    },
    disks: [],
    networks: [],
  }
}

beforeEach(() => {
  FakeWebSocket.reset()
  vi.useFakeTimers()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('PanelSocket', () => {
  it('pide las muestras recién cuando la conexión está lista', () => {
    const panel = new PanelSocket(openFake)

    panel.subscribe(serverA, () => {})
    expect(FakeWebSocket.last.sent).toEqual([])

    FakeWebSocket.last.connect()
    expect(FakeWebSocket.last.requests).toEqual([{ type: 'subscribe', server_ids: [serverA] }])
  })

  it('le da cada muestra al que pidió ese servidor', () => {
    const panel = new PanelSocket(openFake)
    const forA = vi.fn()
    const forB = vi.fn()

    panel.subscribe(serverA, forA)
    panel.subscribe(serverB, forB)
    FakeWebSocket.last.connect()
    FakeWebSocket.last.receive({ type: 'sample', ...sampleFor(serverA) })

    expect(forA).toHaveBeenCalledTimes(1)
    expect(forB).not.toHaveBeenCalled()
  })

  it('recién dice "en vivo" cuando el control plane confirma la suscripción', () => {
    const panel = new PanelSocket(openFake)
    panel.subscribe(serverA, () => {})
    FakeWebSocket.last.connect()

    expect(panel.getStatus()).toBe('connecting')

    FakeWebSocket.last.receive({ type: 'subscribed', server_ids: [serverA] })
    expect(panel.getStatus()).toBe('live')
  })

  it('avisa y se vuelve a suscribir después de una caída', () => {
    const panel = new PanelSocket(openFake)
    const status = vi.fn()
    panel.onStatus(status)
    panel.subscribe(serverA, () => {})
    FakeWebSocket.last.connect()

    FakeWebSocket.last.drop()
    expect(panel.getStatus()).toBe('reconnecting')
    expect(status).toHaveBeenCalledWith('reconnecting')
    expect(FakeWebSocket.sockets).toHaveLength(1)

    vi.advanceTimersByTime(1_000)
    expect(FakeWebSocket.sockets).toHaveLength(2)

    // La conexión nueva no sabe nada: hay que volver a pedir lo mismo.
    FakeWebSocket.last.connect()
    expect(FakeWebSocket.last.requests).toEqual([{ type: 'subscribe', server_ids: [serverA] }])
  })

  it('espera cada vez más entre reintentos', () => {
    const panel = new PanelSocket(openFake)
    panel.subscribe(serverA, () => {})
    FakeWebSocket.last.connect()

    FakeWebSocket.last.drop()
    vi.advanceTimersByTime(1_000)
    expect(FakeWebSocket.sockets).toHaveLength(2)

    FakeWebSocket.last.drop()
    vi.advanceTimersByTime(1_000)
    expect(FakeWebSocket.sockets).toHaveLength(2) // el segundo reintento espera más

    vi.advanceTimersByTime(1_000)
    expect(FakeWebSocket.sockets).toHaveLength(3)
  })

  it('deja de pedir un servidor cuando nadie lo mira', () => {
    const panel = new PanelSocket(openFake)
    const unsubscribe = panel.subscribe(serverA, () => {})
    FakeWebSocket.last.connect()

    unsubscribe()

    expect(FakeWebSocket.last.requests).toEqual([
      { type: 'subscribe', server_ids: [serverA] },
      { type: 'unsubscribe', server_ids: [serverA] },
    ])
  })

  it('no vuelve a conectarse si ya no queda nadie mirando', () => {
    const panel = new PanelSocket(openFake)
    const unsubscribe = panel.subscribe(serverA, () => {})
    FakeWebSocket.last.connect()

    unsubscribe()
    FakeWebSocket.last.drop()
    vi.advanceTimersByTime(60_000)

    expect(FakeWebSocket.sockets).toHaveLength(1)
  })

  it('sigue mandando muestras si dos vistas miran el mismo servidor', () => {
    const panel = new PanelSocket(openFake)
    const first = vi.fn()
    const second = vi.fn()
    const unsubscribe = panel.subscribe(serverA, first)
    panel.subscribe(serverA, second)
    FakeWebSocket.last.connect()

    unsubscribe()
    FakeWebSocket.last.receive({ type: 'sample', ...sampleFor(serverA) })

    expect(first).not.toHaveBeenCalled()
    expect(second).toHaveBeenCalledTimes(1)
    // Con un interesado que queda, no se da de baja el servidor.
    expect(FakeWebSocket.last.requests).toEqual([{ type: 'subscribe', server_ids: [serverA] }])
  })

  it('al cerrar sesión corta el canal y olvida las suscripciones', () => {
    const panel = new PanelSocket(openFake)
    panel.subscribe(serverA, () => {})
    FakeWebSocket.last.connect()

    panel.close()

    expect(FakeWebSocket.last.closedWith).toBe(1000)
    expect(panel.getStatus()).toBe('connecting')

    vi.advanceTimersByTime(60_000)
    expect(FakeWebSocket.sockets).toHaveLength(1)
  })

  it('avisa cuando cambian los proyectos de un servidor', () => {
    const panel = new PanelSocket(openFake)
    const listener = vi.fn()

    panel.subscribeProjects(serverA, listener)
    FakeWebSocket.last.connect()
    FakeWebSocket.last.receive({ type: 'projects', server_id: serverA })
    FakeWebSocket.last.receive({ type: 'projects', server_id: serverB })

    expect(listener).toHaveBeenCalledTimes(1)
    expect(FakeWebSocket.last.requests).toEqual([{ type: 'subscribe', server_ids: [serverA] }])
  })

  it('aguanta un mensaje que no es JSON', () => {
    const panel = new PanelSocket(openFake)
    const listener = vi.fn()
    panel.subscribe(serverA, listener)
    FakeWebSocket.last.connect()

    expect(() => FakeWebSocket.last.onmessage?.({ data: 'no soy JSON' })).not.toThrow()
    expect(listener).not.toHaveBeenCalled()
  })
})
