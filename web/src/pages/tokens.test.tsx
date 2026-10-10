import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { CreateTokenRequest, McpToken, McpTokenList, ProjectSummary } from '@/lib/api'
import { panelSocket } from '@/lib/panel-socket'
import { TokensPage } from '@/pages/tokens'
import { apiError, json, renderWithQuery, stubFetch } from '@/test/render'
import { FakeWebSocket } from '@/test/websocket'

function project(id: string, name: string, environment: ProjectSummary['environment']): ProjectSummary {
  return {
    id,
    name,
    server_id: 's1',
    server_name: 'sv-gcg',
    environment,
    runtime: 'docker',
    repo: `github.com/a/${name}`,
    branch: 'main',
    status: 'ok',
    units: { total: 1, healthy: 1 },
    health: null,
    seen_at: '2026-10-09T12:00:00Z',
  }
}

const projects = [project('p-demo', 'demo', 'staging'), project('p-nbcargo', 'nbcargo', 'production')]

function token(patch: Partial<McpToken> = {}): McpToken {
  return {
    id: 't1',
    name: 'claude-code',
    prefix: 'svmcp_AbCdEf',
    status: 'active',
    scopes: ['read:production', 'logs:production'],
    all_projects: false,
    projects: [{ id: 'p-nbcargo', name: 'nbcargo', server_name: 'sv-gcg', environment: 'production' }],
    created_by_name: 'juan',
    created_at: new Date(Date.now() - 3_600_000).toISOString(),
    expires_at: new Date(Date.now() + 10 * 86_400_000 + 60_000).toISOString(),
    revoked_at: null,
    last_used_at: new Date(Date.now() - 120_000).toISOString(),
    last_used_ip: '203.0.113.7',
    ...patch,
  }
}

function list(tokens: McpToken[]): McpTokenList {
  return {
    tokens,
    max_days: 90,
    default_days: 30,
    scopes: ['read:staging', 'read:production', 'logs:staging', 'logs:production'],
  }
}

/** answer contesta como el control plane, con los tokens que diga state. */
function answer(initial: McpToken[]) {
  const state = { tokens: initial }
  const calls = stubFetch(async (path, init) => {
    if (path === '/v1/projects') return json({ projects, invalid: [], read_errors: [] })
    if (path === '/v1/tokens' && init?.method === 'POST') {
      const req = JSON.parse(String(init.body)) as CreateTokenRequest
      if (req.totp_code !== '123456') return apiError(403, 'invalid_code')
      const created = token({
        id: 't-new',
        name: req.name,
        scopes: req.scopes,
        all_projects: req.project_ids.length === 0,
        projects: [],
        last_used_at: null,
        last_used_ip: undefined,
      })
      state.tokens = [created, ...state.tokens]
      return json({ token: 'svmcp_ValorDelTokenQueSeMuestraUnaSolaVez1234567', item: created }, 201)
    }
    if (path.startsWith('/v1/tokens/') && init?.method === 'DELETE') {
      const id = path.slice('/v1/tokens/'.length)
      state.tokens = state.tokens.map((t) =>
        t.id === id ? { ...t, status: 'revoked', revoked_at: new Date().toISOString(), revoked_by_name: 'juan' } : t,
      )
      return json(state.tokens.find((t) => t.id === id))
    }
    if (path === '/v1/tokens') return json(list(state.tokens))
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

describe('TokensPage', () => {
  it('lista los tokens con sus permisos, sus proyectos y el último uso', async () => {
    answer([token()])
    renderWithQuery(<TokensPage />)

    const item = await screen.findByRole('listitem')
    expect(within(item).getByText('claude-code')).toBeInTheDocument()
    expect(within(item).getByText('svmcp_AbCdEf…')).toBeInTheDocument()
    expect(within(item).getByText('Activo')).toBeInTheDocument()
    expect(within(item).getByText('Leer producción')).toBeInTheDocument()
    expect(within(item).getByText('Logs de producción')).toBeInTheDocument()
    expect(within(item).getByText(/sv-gcg\/nbcargo/)).toBeInTheDocument()
    expect(within(item).getByText(/hace 2 min desde 203\.0\.113\.7/)).toBeInTheDocument()
    expect(within(item).getByText('Vence en 10 días')).toBeInTheDocument()
    expect(within(item).getByRole('link', { name: 'Ver lo que hizo' })).toHaveAttribute(
      'href',
      '/audit?actor_type=llm&actor=claude-code',
    )
  })

  it('un proyecto que ya no está figura así, sin pasar a todos', async () => {
    answer([token({ projects: [{ id: 'p-viejo', missing: true }] })])
    renderWithQuery(<TokensPage />)

    expect(await screen.findByText('un proyecto que ya no está declarado')).toBeInTheDocument()
    expect(screen.queryByText('Todos los proyectos de esos entornos.')).not.toBeInTheDocument()
  })

  it('crea un token y lo muestra una sola vez, con el comando para Claude Code', async () => {
    const user = userEvent.setup()
    const { calls } = answer([])
    renderWithQuery(<TokensPage />)

    await user.click(await screen.findByRole('button', { name: 'Nuevo token' }))
    const form = screen.getByRole('form', { name: 'Nuevo token' })
    const submit = within(form).getByRole('button', { name: 'Crear token' })
    expect(submit).toBeDisabled()

    await user.type(within(form).getByLabelText('Nombre'), 'claude-code')
    await user.click(within(form).getByLabelText('Leer staging'))
    await user.click(within(form).getByLabelText('Logs de staging'))
    // Solo se ofrecen los proyectos de los entornos elegidos.
    await user.click(within(form).getByLabelText('Solo algunos'))
    expect(within(form).getByText('sv-gcg/demo')).toBeInTheDocument()
    expect(within(form).queryByText('sv-gcg/nbcargo')).not.toBeInTheDocument()
    await user.click(within(form).getByRole('checkbox', { name: /sv-gcg\/demo/ }))
    await user.clear(within(form).getByLabelText('Vence en (días)'))
    await user.type(within(form).getByLabelText('Vence en (días)'), '7')
    await user.type(within(form).getByLabelText('Código de la app de autenticación'), '123456')
    expect(submit).toBeEnabled()
    await user.click(submit)

    expect(await screen.findByText('Token creado: claude-code')).toBeInTheDocument()
    expect(screen.getByText('svmcp_ValorDelTokenQueSeMuestraUnaSolaVez1234567')).toBeInTheDocument()
    // El token se muestra una vez y se copia; los comandos no lo llevan.
    expect(screen.getByRole('button', { name: 'Copiar el token' })).toBeInTheDocument()
    expect(
      screen.getByText(
        `claude mcp add --transport http --scope user status-sv ${window.location.origin}/mcp --header 'Authorization: Bearer \${STATUSSV_MCP_TOKEN}'`,
      ),
    ).toBeInTheDocument()
    expect(screen.getByText(/SetEnvironmentVariable\('STATUSSV_MCP_TOKEN', \(Get-Clipboard\), 'User'\)/)).toBeInTheDocument()

    const post = calls.find((c) => c.init?.method === 'POST')
    expect(JSON.parse(String(post?.init?.body))).toEqual({
      name: 'claude-code',
      scopes: ['read:staging', 'logs:staging'],
      project_ids: ['p-demo'],
      expires_in_days: 7,
      totp_code: '123456',
    })

    await user.click(screen.getByRole('button', { name: 'Ya lo copié' }))
    expect(screen.queryByText('svmcp_ValorDelTokenQueSeMuestraUnaSolaVez1234567')).not.toBeInTheDocument()
    expect(await screen.findByText('svmcp_AbCdEf…')).toBeInTheDocument()
  })

  it('con un código que no sirve lo dice y no muestra ningún token', async () => {
    const user = userEvent.setup()
    answer([])
    renderWithQuery(<TokensPage />)

    await user.click(await screen.findByRole('button', { name: 'Nuevo token' }))
    const form = screen.getByRole('form', { name: 'Nuevo token' })
    await user.type(within(form).getByLabelText('Nombre'), 'x')
    await user.click(within(form).getByLabelText('Leer producción'))
    await user.type(within(form).getByLabelText('Código de la app de autenticación'), '000000')
    await user.click(within(form).getByRole('button', { name: 'Crear token' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('hace falta uno nuevo de la app de autenticación')
    expect(screen.queryByText(/Token creado/)).not.toBeInTheDocument()
  })

  it('revoca con una confirmación', async () => {
    const user = userEvent.setup()
    const { calls } = answer([token()])
    renderWithQuery(<TokensPage />)

    await user.click(await screen.findByRole('button', { name: 'Revocar claude-code' }))
    await user.click(screen.getByRole('button', { name: 'Sí, revocar' }))

    await waitFor(() => expect(screen.getAllByText('Revocado').length).toBeGreaterThan(0))
    expect(calls.some((c) => c.init?.method === 'DELETE' && c.path === '/v1/tokens/t1')).toBe(true)
    expect(screen.queryByRole('button', { name: 'Revocar claude-code' })).not.toBeInTheDocument()
  })
})
