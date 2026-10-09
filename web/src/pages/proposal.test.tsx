import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { ProjectSummary, ProposalDetail, ServerSummary } from '@/lib/api'
import { panelSocket } from '@/lib/panel-socket'
import { ProposalPage } from '@/pages/proposal'
import { apiError, json, renderWithQuery, stubFetch } from '@/test/render'
import { FakeWebSocket } from '@/test/websocket'

const serverID = '01924f9c-0000-7000-8000-0000000000aa'
const proposalID = '01924f9c-0000-7000-8000-0000000000bb'

const server: ServerSummary = {
  id: serverID,
  name: 'sv-gcg',
  hostname: 'gcg.example',
  labels: {},
  agent_version: '0.6.0',
  status: 'online',
  last_seen_at: '2026-10-09T12:00:00Z',
  enrolled_at: '2026-09-01T00:00:00Z',
  sample: null,
}

function proposal(patch: Partial<ProposalDetail> = {}): ProposalDetail {
  return {
    id: proposalID,
    server_id: serverID,
    name: 'svdemo',
    runtime: 'systemd',
    environment: 'staging',
    status: 'delivered',
    created_by: 'juan',
    actor_type: 'user',
    created_at: new Date().toISOString(),
    expires_at: new Date(Date.now() + 7 * 24 * 3_600_000).toISOString(),
    command: 'sudo statussv-agent project accept svdemo',
    preview: {
      yaml: 'name: svdemo\n',
      unit: '[Service]\nUser=svdemo\n',
      command: 'sudo statussv-agent project accept svdemo',
    },
    ...patch,
  }
}

const project: ProjectSummary = {
  id: 'p-svdemo',
  name: 'svdemo',
  server_id: serverID,
  server_name: 'sv-gcg',
  environment: 'staging',
  runtime: 'systemd',
  repo: 'github.com/JuanBaez77/status-sv-demo',
  branch: 'main',
  status: 'down',
  units: { total: 1, healthy: 0 },
  health: null,
  seen_at: '2026-10-09T12:00:00Z',
}

/** answer responde con la propuesta que diga current, que la prueba puede cambiar. */
function answer(initial: ProposalDetail) {
  const state = { current: initial }
  const calls = stubFetch((path, init) => {
    if (path === '/v1/servers') return json({ servers: [server] })
    if (path === '/v1/projects') return json({ projects: [project], invalid: [], read_errors: [] })
    if (path === `/v1/proposals/${proposalID}` && init?.method === 'DELETE') {
      state.current = { ...state.current, status: 'withdrawn' }
      return json(state.current)
    }
    if (path === `/v1/proposals/${proposalID}`) return json(state.current)
    return apiError(404, 'not_found')
  })
  return { state, calls }
}

beforeEach(() => {
  FakeWebSocket.reset()
  vi.stubGlobal('WebSocket', FakeWebSocket)
})

afterEach(() => {
  panelSocket.close()
  vi.unstubAllGlobals()
})

describe('ProposalPage', () => {
  it('abierta, muestra el comando para el servidor y lo que va a escribir', async () => {
    answer(proposal())
    renderWithQuery(<ProposalPage id={proposalID} />)

    expect(await screen.findByText('Esperando que la acepten en el servidor')).toBeInTheDocument()
    expect(screen.getByText('sudo statussv-agent project accept svdemo')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Copiar el comando de aceptación' })).toBeInTheDocument()
    expect(screen.getByText(/User=svdemo/)).toBeInTheDocument()
    expect(screen.getByText(/Vence en 7 días/)).toBeInTheDocument()
  })

  it('se puede retirar', async () => {
    const user = userEvent.setup()
    const { calls } = answer(proposal())
    renderWithQuery(<ProposalPage id={proposalID} />)

    await user.click(await screen.findByRole('button', { name: 'Retirar la propuesta' }))
    expect(await screen.findByText('Retirada', { selector: 'span' })).toBeInTheDocument()
    expect(calls.some((c) => c.init?.method === 'DELETE')).toBe(true)
    expect(screen.queryByText('sudo statussv-agent project accept svdemo')).not.toBeInTheDocument()
  })

  it('se entera sola cuando la aceptan en el servidor', async () => {
    const { state } = answer(proposal())
    renderWithQuery(<ProposalPage id={proposalID} />)
    await screen.findByText('Esperando que la acepten en el servidor')
    const socket = FakeWebSocket.last
    act(() => socket.connect())

    state.current = proposal({ status: 'accepted', message: 'declarada igual a la propuesta' })
    act(() => socket.receive({ type: 'proposals', server_id: serverID }))

    expect(await screen.findByText('Aceptada en sv-gcg')).toBeInTheDocument()
    expect(screen.getByText(/Quedó declarada tal cual se propuso/)).toBeInTheDocument()
    await waitFor(() => expect(screen.getByRole('link', { name: 'Ir al proyecto' })).toHaveAttribute('href', '/projects/p-svdemo'))
  })

  it('rechazada, dice por qué y ofrece proponer de nuevo', async () => {
    answer(proposal({ status: 'rejected', message: 'ya hay 10 propuestas guardadas' }))
    renderWithQuery(<ProposalPage id={proposalID} />)

    expect(await screen.findByText(/El agent dijo: ya hay 10 propuestas guardadas/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Nuevo proyecto' })).toHaveAttribute('href', `/servers/${serverID}/new-project`)
  })
})
