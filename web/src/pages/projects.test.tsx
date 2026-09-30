import { screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { ProjectList, ProjectSummary } from '@/lib/api'
import { panelSocket } from '@/lib/panel-socket'
import { ProjectsPage } from '@/pages/projects'
import { json, renderWithQuery, stubFetch } from '@/test/render'
import { FakeWebSocket } from '@/test/websocket'

function project(name: string, status: ProjectSummary['status']): ProjectSummary {
  return {
    id: `id-${name}`,
    name,
    server_id: 's1',
    server_name: 'vps-cristales',
    environment: 'production',
    runtime: 'docker',
    repo: `github.com/x/${name}`,
    branch: 'main',
    status,
    units: { total: 1, healthy: status === 'ok' ? 1 : 0 },
    health: null,
    seen_at: '2026-09-28T12:00:00Z',
  }
}

beforeEach(() => {
  FakeWebSocket.reset()
  vi.stubGlobal('WebSocket', FakeWebSocket)
})

afterEach(() => {
  panelSocket.close()
  vi.unstubAllGlobals()
})

describe('ProjectsPage', () => {
  it('muestra primero lo que tiene problemas, y los archivos inválidos', async () => {
    const list: ProjectList = {
      projects: [project('web', 'ok'), project('api', 'down'), project('facturas', 'degraded')],
      invalid: [
        {
          server_id: 's1',
          server_name: 'vps-cristales',
          file: 'roto.yaml',
          error: 'name: falta',
          seen_at: '2026-09-28T12:00:00Z',
        },
      ],
      read_errors: [],
    }
    stubFetch(() => json(list))

    renderWithQuery(<ProjectsPage />)

    const items = await screen.findAllByRole('link')
    expect(items.map((item) => within(item).getByText(/^(web|api|facturas)$/).textContent)).toEqual(
      ['api', 'facturas', 'web'],
    )
    expect(screen.getByText('Caído')).toBeInTheDocument()
    expect(screen.getByText('roto.yaml')).toBeInTheDocument()
    expect(screen.getByText('name: falta')).toBeInTheDocument()
  })

  it('sin proyectos explica dónde se declaran', async () => {
    stubFetch(() => json({ projects: [], invalid: [], read_errors: [] }))

    renderWithQuery(<ProjectsPage />)

    expect(await screen.findByText('Todavía no hay proyectos declarados')).toBeInTheDocument()
  })

  it('avisa si no se pudo leer el directorio de un servidor', async () => {
    stubFetch(() =>
      json({
        projects: [],
        invalid: [],
        read_errors: [
          {
            server_id: 's1',
            server_name: 'vps-cristales',
            detail: 'permiso denegado',
            taken_at: null,
          },
        ],
      }),
    )

    renderWithQuery(<ProjectsPage />)

    expect(await screen.findByRole('alert')).toHaveTextContent('permiso denegado')
  })
})
