import { screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { ProjectDetail } from '@/lib/api'
import { panelSocket } from '@/lib/panel-socket'
import { ProjectPage } from '@/pages/project'
import { json, renderWithQuery, stubFetch } from '@/test/render'
import { FakeWebSocket } from '@/test/websocket'

const key = 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAbCdEfGhIjKlMnOpQrStUvWxYz0123456789abcdefgh'

function project(overrides: Partial<ProjectDetail> = {}): ProjectDetail {
  return {
    id: 'p1',
    name: 'svdemo',
    server_id: 's1',
    server_name: 'sv-gcg',
    environment: 'staging',
    runtime: 'systemd',
    repo: 'github.com/JuanBaez77/status-sv-demo',
    branch: 'main',
    status: 'down',
    units: { total: 1, healthy: 0 },
    health: null,
    seen_at: '2026-10-09T12:00:00Z',
    workdir: '/srv/svdemo',
    declared_units: ['svdemo.service'],
    missing_units: [],
    containers: [],
    services: [],
    source: null,
    ...overrides,
  }
}

function answer(p: ProjectDetail) {
  stubFetch((path) => {
    if (path === '/v1/projects/p1/deploys') return json({ deploys: [] })
    return json(p)
  })
}

beforeEach(() => {
  FakeWebSocket.reset()
  vi.stubGlobal('WebSocket', FakeWebSocket)
})

afterEach(() => {
  panelSocket.close()
  vi.unstubAllGlobals()
})

describe('ProjectPage: lo del alta', () => {
  it('muestra las variables que faltan, la llave y el sitio', async () => {
    answer(
      project({
        env: { required: 2, missing: ['SECRET_KEY'], unknown: false },
        deploy_key: key,
        site: { domain: 'svdemo.extendsystem.com', port: 18097 },
      }),
    )
    renderWithQuery(<ProjectPage id="p1" />)

    expect(await screen.findByText('En el servidor')).toBeInTheDocument()
    expect(await screen.findByRole('alert')).toHaveTextContent('Faltan 1 de 2: SECRET_KEY.')
    expect(screen.getByText('sudo statussv-agent project env svdemo')).toBeInTheDocument()
    expect(screen.getByText(key)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Copiar la llave pública' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'svdemo.extendsystem.com' })).toHaveAttribute('href', 'https://svdemo.extendsystem.com')
  })

  it('con todo cargado, no pide nada', async () => {
    answer(project({ env: { required: 2, missing: [], unknown: false } }))
    renderWithQuery(<ProjectPage id="p1" />)

    expect(await screen.findByText('Están cargadas las 2 variables requeridas.')).toBeInTheDocument()
    expect(screen.queryByText('sudo statussv-agent project env svdemo')).not.toBeInTheDocument()
  })

  it('un proyecto declarado a mano no tiene esa tarjeta', async () => {
    answer(project())
    renderWithQuery(<ProjectPage id="p1" />)

    expect(await screen.findByText('Declarado')).toBeInTheDocument()
    expect(screen.queryByText('En el servidor')).not.toBeInTheDocument()
  })
})
