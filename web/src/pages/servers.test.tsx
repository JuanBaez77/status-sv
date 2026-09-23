import { screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { ServerSummary } from '@/lib/api'
import { ServersPage } from '@/pages/servers'
import { apiError, json, renderWithQuery, stubFetch } from '@/test/render'

afterEach(() => {
  vi.unstubAllGlobals()
})

function server(overrides: Partial<ServerSummary> = {}): ServerSummary {
  return {
    id: '01924f9c-0000-7000-8000-0000000000aa',
    name: 'vps-01',
    hostname: 'vps-01.example',
    labels: {},
    agent_version: '0.1.0',
    status: 'online',
    last_seen_at: new Date().toISOString(),
    enrolled_at: '2026-09-01T00:00:00Z',
    sample: {
      ts: new Date().toISOString(),
      cpu_used_percent: 12.5,
      cpu_cores: 4,
      mem_total_bytes: 8 * 1024 ** 3,
      mem_used_bytes: 2 * 1024 ** 3,
      load1: 0.42,
      worst_disk: {
        mountpoint: '/',
        total_bytes: 100 * 1024 ** 3,
        used_bytes: 95 * 1024 ** 3,
        available_bytes: 5 * 1024 ** 3,
        used_percent: 95,
      },
    },
    ...overrides,
  }
}

describe('ServersPage', () => {
  it('muestra cada servidor con su estado y sus métricas', async () => {
    stubFetch(() =>
      json({
        servers: [
          server(),
          server({
            id: '01924f9c-0000-7000-8000-0000000000bb',
            name: 'vps-02',
            status: 'offline',
            last_seen_at: '2026-09-22T10:00:00Z',
            sample: null,
          }),
        ],
      }),
    )

    renderWithQuery(<ServersPage />)

    expect(await screen.findByText('vps-01')).toBeInTheDocument()
    expect(screen.getByText('En línea')).toBeInTheDocument()
    expect(screen.getByText('vps-02')).toBeInTheDocument()
    expect(screen.getByText('Caído')).toBeInTheDocument()

    // El uso se muestra como barra con su valor accesible.
    expect(screen.getByRole('meter', { name: 'CPU' })).toHaveAttribute('aria-valuenow', '13')
    expect(screen.getByRole('meter', { name: 'Memoria' })).toHaveAttribute('aria-valuenow', '25')
    expect(screen.getByRole('meter', { name: 'Disco /' })).toHaveAttribute('aria-valuenow', '95')
    expect(screen.getByText('quedan 5 GiB')).toBeInTheDocument()

    // El que no mandó métricas lo dice en vez de mostrar ceros.
    expect(screen.getByText(/Sin métricas todavía/)).toBeInTheDocument()
  })

  it('un servidor sin servidores explica cómo sumar uno', async () => {
    stubFetch(() => json({ servers: [] }))

    renderWithQuery(<ServersPage />)

    expect(await screen.findByText('Todavía no hay servidores')).toBeInTheDocument()
    expect(screen.getByText(/enroll-token/)).toBeInTheDocument()
  })

  it('avisa si la API falla', async () => {
    stubFetch(() => apiError(500, 'internal', 'error interno'))

    renderWithQuery(<ServersPage />)

    expect(await screen.findByText('No se pudieron traer los servidores')).toBeInTheDocument()
  })

  it('cada servidor enlaza a su vista', async () => {
    stubFetch(() => json({ servers: [server()] }))

    renderWithQuery(<ServersPage />)

    const link = await screen.findByRole('link', { name: 'vps-01' })
    expect(link).toHaveAttribute('href', '/servers/01924f9c-0000-7000-8000-0000000000aa')
  })
})
