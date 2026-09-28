import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { ServerLogs, ServerMetrics, ServerRuntime, ServerSummary } from '@/lib/api'
import { panelSocket } from '@/lib/panel-socket'
import { ServerPage } from '@/pages/server'
import { json, renderWithQuery, stubFetch } from '@/test/render'
import { FakeWebSocket } from '@/test/websocket'

// Los gráficos se prueban aparte: acá lo que importa es qué se arma con los
// datos, no lo que dibuja el canvas.
vi.mock('@/components/chart', () => ({
  Chart: ({ label }: { label: string }) => <div data-testid="chart">{label}</div>,
}))

const id = '01924f9c-0000-7000-8000-0000000000aa'
const gib = 1024 ** 3

function summary(overrides: Partial<ServerSummary> = {}): ServerSummary {
  return {
    id,
    name: 'vps-cristales',
    hostname: 'cristales.example',
    labels: {},
    agent_version: '0.1.0',
    status: 'online',
    last_seen_at: new Date().toISOString(),
    enrolled_at: '2026-09-01T00:00:00Z',
    sample: null,
    ...overrides,
  }
}

function series(overrides: Partial<ServerMetrics> = {}): ServerMetrics {
  return {
    server_id: id,
    resolution: 'raw',
    from: '2026-09-23T11:00:00Z',
    to: '2026-09-23T12:00:00Z',
    truncated: false,
    samples: [
      {
        ts: '2026-09-23T12:00:00Z',
        cpu_used_percent: 12.5,
        cpu_used_max_percent: 12.5,
        cpu_iowait_percent: 0.5,
        cpu_iowait_max_percent: 0.5,
        cpu_steal_percent: 0,
        cpu_cores: 4,
        mem_total_bytes: 8 * gib,
        mem_used_bytes: 2 * gib,
        mem_used_max_bytes: 2 * gib,
        swap_total_bytes: 0,
        swap_used_bytes: 0,
        swap_used_max_bytes: 0,
        load1: 0.42,
        load5: 0.3,
        load15: 0.2,
      },
    ],
    disks: [
      {
        mountpoint: '/',
        points: [
          {
            ts: '2026-09-23T12:00:00Z',
            total_bytes: 100 * gib,
            used_bytes: 93 * gib,
            available_bytes: 7 * gib,
          },
        ],
      },
    ],
    networks: [
      {
        interface: 'eth0',
        points: [
          {
            ts: '2026-09-23T12:00:00Z',
            rx_bytes_per_sec: 2048,
            rx_max_bytes_per_sec: 2048,
            tx_bytes_per_sec: 1024,
            tx_max_bytes_per_sec: 1024,
          },
        ],
      },
    ],
    ...overrides,
  }
}

function runtime(overrides: Partial<ServerRuntime> = {}): ServerRuntime {
  const seen = '2026-09-23T12:00:00Z'
  return {
    server_id: id,
    taken_at: seen,
    sources: {
      docker: { status: 'ok', omitted: 0, taken_at: seen },
      systemd: { status: 'ok', omitted: 0, taken_at: seen },
      processes: { status: 'ok', omitted: 0, taken_at: seen },
    },
    containers: [
      {
        id: 'a'.repeat(64),
        name: 'cristales-web-1',
        image: 'ghcr.io/extendsystem/cristales:2026.09.20',
        state: 'running',
        health: 'healthy',
        started_at: seen,
        compose_project: 'cristales',
        compose_service: 'web',
        usage: { cpu_percent: 1.5, memory_bytes: 180 * 1024 ** 2 },
        seen_at: seen,
      },
      {
        id: 'b'.repeat(64),
        name: 'cristales-worker-1',
        image: 'ghcr.io/extendsystem/cristales:2026.09.20',
        state: 'restarting',
        health: '',
        started_at: null,
        compose_project: 'cristales',
        compose_service: 'worker',
        usage: null,
        seen_at: seen,
      },
      {
        id: 'c'.repeat(64),
        name: 'migracion-vieja',
        image: 'busybox',
        state: 'exited',
        health: '',
        started_at: null,
        compose_project: '',
        compose_service: '',
        usage: null,
        seen_at: seen,
      },
    ],
    services: [
      {
        unit: 'nginx.service',
        load_state: 'loaded',
        active_state: 'active',
        sub_state: 'running',
        main_pid: 812,
        active_since: seen,
        usage: { cpu_percent: 0.1, memory_bytes: 12 * 1024 ** 2 },
        seen_at: seen,
      },
    ],
    processes: [
      {
        pid: 900,
        name: 'python3',
        command: 'python3 importar.py --db-password=***',
        user: 'juan',
        started_at: seen,
        usage: { cpu_percent: 25, memory_bytes: 100 * 1024 ** 2 },
        seen_at: seen,
      },
    ],
    ...overrides,
  }
}

/** answer responde la lista, las métricas y lo que corre, y guarda las consultas de rango. */
function answer(
  options: { servers?: ServerSummary[]; metrics?: ServerMetrics; runtime?: ServerRuntime; logs?: ServerLogs } = {},
) {
  const asked: URL[] = []
  const calls = stubFetch((path) => {
    if (path === '/v1/servers') {
      return json({ servers: options.servers ?? [summary()] })
    }
    if (path.endsWith('/runtime')) {
      return json(options.runtime ?? runtime())
    }
    if (path.endsWith('/logs')) {
      return json(
        options.logs ?? {
          status: 'ok',
          truncated: false,
          lines: [
            { ts: '2026-09-23T12:00:00Z', stream: 'stdout', text: 'escuchando en :8000' },
            { ts: '2026-09-23T12:00:01Z', stream: 'stderr', text: 'conexión a postgres://app:***@db rechazada' },
          ],
        },
      )
    }
    asked.push(new URL(path, 'http://panel.test'))
    return json(options.metrics ?? series())
  })
  return { asked, calls }
}

beforeEach(() => {
  FakeWebSocket.reset()
  vi.stubGlobal('WebSocket', FakeWebSocket)
  window.history.replaceState({}, '', '/')
})

afterEach(() => {
  panelSocket.close()
  vi.unstubAllGlobals()
})

describe('ServerPage', () => {
  it('muestra el estado de ahora y un gráfico por familia de métrica', async () => {
    answer()

    renderWithQuery(<ServerPage id={id} />)

    expect(await screen.findByRole('heading', { name: 'vps-cristales' })).toBeInTheDocument()
    expect(screen.getByText('En línea')).toBeInTheDocument()
    expect(screen.getByText('12,5 %')).toBeInTheDocument() // CPU
    expect(screen.getByText('0,42')).toBeInTheDocument() // carga de 1 minuto
    expect(screen.getByText('2 KiB/s')).toBeInTheDocument() // entrada de red

    // El disco más lleno se ve como barra, con su valor accesible.
    expect(screen.getByRole('meter', { name: '/' })).toHaveAttribute('aria-valuenow', '93')

    expect(screen.getAllByTestId('chart').map((node) => node.textContent)).toEqual([
      'Uso de CPU',
      'Memoria en uso',
      'Carga del sistema',
      'Uso de disco',
      'Tráfico de eth0',
    ])
  })

  it('una muestra que llega en vivo actualiza los números', async () => {
    answer()

    renderWithQuery(<ServerPage id={id} />)
    await screen.findByText('12,5 %')

    act(() => {
      FakeWebSocket.last.connect()
      FakeWebSocket.last.receive({ type: 'subscribed', server_ids: [id] })
      FakeWebSocket.last.receive({
        type: 'sample',
        server_id: id,
        sample: { ...series().samples[0], ts: '2026-09-23T12:00:10Z', cpu_used_percent: 71.5 },
        disks: [],
        networks: [],
      })
    })

    expect(await screen.findByText('71,5 %')).toBeInTheDocument()
    expect(screen.getByText('En vivo')).toBeInTheDocument()
  })

  it('pide a la API el rango que se elige', async () => {
    const { asked } = answer()

    renderWithQuery(<ServerPage id={id} />)
    await screen.findByRole('heading', { name: 'vps-cristales' })

    const primero = span(asked.at(-1)!)
    expect(primero).toBeCloseTo(60 * 60, 0)

    await userEvent.click(screen.getByRole('button', { name: '24 horas' }))

    await waitFor(() => expect(span(asked.at(-1)!)).toBeCloseTo(24 * 60 * 60, 0))
    // El rango queda en la dirección: el enlace que se comparte lo lleva puesto.
    expect(new URLSearchParams(window.location.search).get('rango')).toBe('24h')
  })

  it('dice cuándo el rango elegido no tiene métricas', async () => {
    answer({ metrics: series({ samples: [], disks: [], networks: [] }) })

    renderWithQuery(<ServerPage id={id} />)

    expect(await screen.findByText('No hay métricas en este rango')).toBeInTheDocument()
    expect(screen.queryAllByTestId('chart')).toHaveLength(0)
  })

  it('muestra lo que corre, con lo que tiene problemas primero', async () => {
    answer()

    renderWithQuery(<ServerPage id={id} />)

    const containers = await screen.findByRole('heading', { name: 'Contenedores' })
    const card = containers.closest('div.rounded-lg') as HTMLElement
    const names = [...card.querySelectorAll('li span[title]')].map((node) => node.textContent)
    // El que se está reiniciando va antes que el sano, y el detenido no se ve.
    expect(names).toEqual(['cristales-worker-1', 'cristales-web-1'])
    expect(screen.getByText('1 corriendo · 1 para revisar · 1 detenido')).toBeInTheDocument()

    // Los detenidos aparecen cuando se piden.
    await userEvent.click(screen.getByRole('button', { name: 'Mostrar 1 detenido' }))
    expect(screen.getByText('migracion-vieja')).toBeInTheDocument()

    expect(screen.getByText('nginx')).toBeInTheDocument()
    expect(screen.getByText('python3 importar.py --db-password=***')).toBeInTheDocument()
  })

  it('explica cuando una fuente no existe o no se pudo leer', async () => {
    const seen = '2026-09-23T12:00:00Z'
    answer({
      runtime: runtime({
        sources: {
          docker: { status: 'unavailable', detail: 'Docker no está instalado en este servidor', omitted: 0, taken_at: seen },
          systemd: { status: 'error', detail: 'tiempo agotado', omitted: 0, taken_at: seen },
          processes: { status: 'ok', omitted: 0, taken_at: seen },
        },
        containers: [],
      }),
    })

    renderWithQuery(<ServerPage id={id} />)

    expect(await screen.findByText('Docker no está instalado en este servidor')).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('No se pudo leer: tiempo agotado')
  })

  it('una foto que llega en vivo reemplaza lo que se ve', async () => {
    answer()

    renderWithQuery(<ServerPage id={id} />)
    await screen.findByText('cristales-web-1')

    act(() => {
      FakeWebSocket.last.connect()
      FakeWebSocket.last.receive({ type: 'subscribed', server_ids: [id] })
      FakeWebSocket.last.receive({
        type: 'runtime',
        server_id: id,
        runtime: runtime({ containers: [{ ...runtime().containers[0], name: 'cristales-web-2' }] }),
      })
    })

    expect(await screen.findByText('cristales-web-2')).toBeInTheDocument()
    expect(screen.queryByText('cristales-web-1')).not.toBeInTheDocument()
  })

  it('avisa si el agent todavía no mandó lo que corre', async () => {
    answer({ runtime: runtime({ taken_at: null, sources: {}, containers: [], services: [], processes: [] }) })

    renderWithQuery(<ServerPage id={id} />)

    expect(await screen.findByText(/todavía no mandó lo que corre/)).toBeInTheDocument()
  })

  it('trae los logs de un contenedor al abrirlos', async () => {
    const { calls } = answer()

    renderWithQuery(<ServerPage id={id} />)
    await userEvent.click(await screen.findByRole('button', { name: 'Ver los logs de cristales-web-1' }))

    expect(await screen.findByText('escuchando en :8000')).toBeInTheDocument()
    const error = screen.getByText('conexión a postgres://app:***@db rechazada')
    expect(error.closest('li')).toHaveClass('text-destructive')
    expect(screen.getByText('Últimos 2 renglones')).toBeInTheDocument()

    const pedido = calls.find((call) => call.path.endsWith('/logs'))
    expect(pedido?.init?.method).toBe('POST')
    expect(JSON.parse(String(pedido?.init?.body))).toEqual({ kind: 'container', target: 'a'.repeat(64), lines: 200 })
  })

  it('explica por qué el agent no trajo los logs', async () => {
    answer({ logs: { status: 'rejected', code: 'busy', lines: [], truncated: false } })

    renderWithQuery(<ServerPage id={id} />)
    await userEvent.click(await screen.findByRole('button', { name: 'Ver los logs de nginx' }))

    expect(await screen.findByText(/El agent está atendiendo otros pedidos/)).toBeInTheDocument()
  })

  it('avisa si el servidor no existe', async () => {
    answer({ servers: [] })

    renderWithQuery(<ServerPage id={id} />)

    expect(await screen.findByText('No existe ese servidor')).toBeInTheDocument()
  })
})

/** span son los segundos que abarca una consulta de métricas. */
function span(url: URL): number {
  const from = Date.parse(url.searchParams.get('from') ?? '')
  const to = Date.parse(url.searchParams.get('to') ?? '')
  return (to - from) / 1000
}
