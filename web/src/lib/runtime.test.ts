import { describe, expect, it } from 'vitest'

import type { RuntimeContainer, RuntimeService } from '@/lib/api'
import {
  byAttention,
  containerStatus,
  serviceStatus,
  sourceNotice,
  summarize,
  summaryText,
} from '@/lib/runtime'

function container(overrides: Partial<RuntimeContainer>): RuntimeContainer {
  return {
    id: 'a'.repeat(64),
    name: 'web',
    image: 'nginx:1.27',
    state: 'running',
    health: '',
    started_at: null,
    compose_project: '',
    compose_service: '',
    usage: null,
    seen_at: '2026-09-28T00:00:00Z',
    ...overrides,
  }
}

function service(overrides: Partial<RuntimeService>): RuntimeService {
  return {
    unit: 'nginx.service',
    load_state: 'loaded',
    active_state: 'active',
    sub_state: 'running',
    main_pid: 812,
    active_since: null,
    usage: null,
    seen_at: '2026-09-28T00:00:00Z',
    ...overrides,
  }
}

describe('containerStatus', () => {
  it.each([
    [{ state: 'running', health: '' }, 'ok', 'Corriendo'],
    [{ state: 'running', health: 'healthy' }, 'ok', 'Saludable'],
    [{ state: 'running', health: 'unhealthy' }, 'bad', 'No saludable'],
    [{ state: 'running', health: 'starting' }, 'warning', 'Arrancando'],
    [{ state: 'restarting' }, 'warning', 'Reiniciándose'],
    [{ state: 'dead' }, 'bad', 'Muerto'],
    // Un contenedor detenido no es un problema por sí solo: puede ser una tarea que terminó.
    [{ state: 'exited' }, 'idle', 'Detenido'],
    [{ state: 'unknown' }, 'idle', 'Desconocido'],
  ] as const)('%o es %s (%s)', (overrides, tone, label) => {
    expect(containerStatus(container(overrides))).toEqual({ tone, label })
  })
})

describe('serviceStatus', () => {
  it.each([
    ['active', 'ok'],
    ['failed', 'bad'],
    // Parado y habilitado: arranca con el sistema y no está corriendo.
    ['inactive', 'warning'],
    ['activating', 'warning'],
    ['unknown', 'idle'],
  ] as const)('%s es %s', (active_state, tone) => {
    expect(serviceStatus(service({ active_state })).tone).toBe(tone)
  })
})

describe('byAttention', () => {
  it('pone primero lo roto, después lo que está en transición y ordena por nombre', () => {
    const list = [
      container({ name: 'b-sano' }),
      container({ name: 'viejo', state: 'exited' }),
      container({ name: 'roto', state: 'dead' }),
      container({ name: 'a-sano' }),
      container({ name: 'volviendo', state: 'restarting' }),
    ]
    expect(byAttention(list, containerStatus, (c) => c.name).map((c) => c.name)).toEqual([
      'roto',
      'volviendo',
      'a-sano',
      'b-sano',
      'viejo',
    ])
  })
})

describe('summaryText', () => {
  it('resume en una línea y omite lo que está en cero', () => {
    const summary = summarize(
      [container({}), container({}), container({ state: 'dead' }), container({ state: 'exited' })],
      containerStatus,
    )
    expect(summaryText(summary, 'corriendo', 'detenidos')).toBe('2 corriendo · 1 con problemas · 1 detenidos')
  })

  it('dice algo cuando no hay nada', () => {
    expect(summaryText(summarize([], containerStatus), 'corriendo', 'detenidos')).toBe('Nada por ahora.')
  })
})

describe('sourceNotice', () => {
  const base = { omitted: 0, taken_at: '2026-09-28T00:00:00Z' }

  it('no avisa nada si la fuente está bien', () => {
    expect(sourceNotice({ ...base, status: 'ok' }, 'Docker')).toEqual({ kind: 'none' })
  })

  it('explica que el servidor no tiene la fuente', () => {
    expect(
      sourceNotice({ ...base, status: 'unavailable', detail: 'Docker no está instalado en este servidor' }, 'Docker'),
    ).toEqual({ kind: 'unavailable', text: 'Docker no está instalado en este servidor' })
  })

  it('avisa el error y desde cuándo lo que se ve es viejo', () => {
    expect(sourceNotice({ ...base, status: 'error', detail: 'tiempo agotado' }, 'Docker')).toEqual({
      kind: 'error',
      text: 'tiempo agotado',
      since: base.taken_at,
    })
  })

  it('avisa si quedaron unidades afuera', () => {
    expect(sourceNotice({ ...base, status: 'ok', omitted: 3 }, 'Docker').kind).toBe('omitted')
  })

  it('sin fuente todavía, no avisa nada', () => {
    expect(sourceNotice(undefined, 'Docker')).toEqual({ kind: 'none' })
  })
})
