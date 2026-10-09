import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { ProposalDetail, ProposalPreview, ServerSummary } from '@/lib/api'
import { panelSocket } from '@/lib/panel-socket'
import { NewProjectPage } from '@/pages/new-project'
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

const preview: ProposalPreview = {
  yaml: 'name: svdemo\nworkdir: /srv/svdemo\n',
  unit: '[Service]\nUser=svdemo\n',
  site: 'server_name svdemo.extendsystem.com;\n',
  command: 'sudo statussv-agent project accept svdemo',
}

/** answer responde el servidor, la vista previa y la propuesta nueva. */
function answer(options: { previewError?: Response } = {}) {
  return stubFetch((path, init) => {
    if (path === '/v1/servers') return json({ servers: [server] })
    if (path.endsWith('/proposals/preview')) return options.previewError ?? json(preview)
    if (path.endsWith('/proposals') && init?.method === 'POST') {
      const created: ProposalDetail = {
        id: proposalID,
        server_id: serverID,
        name: 'svdemo',
        runtime: 'systemd',
        environment: 'staging',
        status: 'pending',
        created_by: 'juan',
        actor_type: 'user',
        created_at: '2026-10-09T12:00:00Z',
        expires_at: '2026-10-16T12:00:00Z',
        command: preview.command,
        preview,
      }
      return json(created, 201)
    }
    return json({}, 404)
  })
}

async function fillSystemd(user: ReturnType<typeof userEvent.setup>) {
  await user.type(await screen.findByLabelText('Nombre'), 'svdemo')
  await user.type(screen.getByLabelText('Repositorio'), 'github.com/JuanBaez77/status-sv-demo')
  await user.type(screen.getByLabelText('Comando del servicio'), '.venv/bin/gunicorn app:app --bind 127.0.0.1:18097')
  await user.type(screen.getByLabelText('Variables requeridas'), 'SECRET_KEY')
  await user.type(screen.getByLabelText('Dominio del sitio (opcional)'), 'svdemo.extendsystem.com')
  await user.type(screen.getByLabelText('Puerto del proyecto'), '18097')
}

beforeEach(() => {
  FakeWebSocket.reset()
  vi.stubGlobal('WebSocket', FakeWebSocket)
  window.history.replaceState({}, '', `/servers/${serverID}/new-project`)
})

afterEach(() => {
  panelSocket.close()
  vi.unstubAllGlobals()
})

describe('NewProjectPage', () => {
  it('muestra lo que se va a escribir y después propone', async () => {
    const user = userEvent.setup()
    const calls = answer()
    renderWithQuery(<NewProjectPage serverID={serverID} />)

    await fillSystemd(user)
    // Sin mirar la vista previa no se puede proponer.
    expect(screen.queryByRole('button', { name: 'Proponer' })).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Ver lo que se va a escribir' }))
    expect(await screen.findByText(/User=svdemo/)).toBeInTheDocument()
    expect(screen.getByText(/server_name svdemo.extendsystem.com/)).toBeInTheDocument()

    const previewCall = calls.find((c) => c.path.endsWith('/proposals/preview'))
    expect(JSON.parse(String(previewCall?.init?.body))).toEqual({
      name: 'svdemo',
      environment: 'staging',
      repo: 'github.com/JuanBaez77/status-sv-demo',
      branch: 'main',
      runtime: 'systemd',
      command: ['.venv/bin/gunicorn', 'app:app', '--bind', '127.0.0.1:18097'],
      required_env: ['SECRET_KEY'],
      site: { domain: 'svdemo.extendsystem.com', port: 18097 },
    })

    await user.click(screen.getByRole('button', { name: 'Proponer' }))
    await waitFor(() => expect(window.location.pathname).toBe(`/proposals/${proposalID}`))
    const created = calls.find((c) => c.path === `/v1/servers/${serverID}/proposals` && c.init?.method === 'POST')
    expect(created?.init?.body).toBe(previewCall?.init?.body)
  })

  it('si cambia el formulario, hay que volver a mirar', async () => {
    const user = userEvent.setup()
    answer()
    renderWithQuery(<NewProjectPage serverID={serverID} />)

    await fillSystemd(user)
    await user.click(screen.getByRole('button', { name: 'Ver lo que se va a escribir' }))
    expect(await screen.findByRole('button', { name: 'Proponer' })).toBeInTheDocument()

    await user.type(screen.getByLabelText('Rama'), '-2')
    expect(screen.queryByRole('button', { name: 'Proponer' })).not.toBeInTheDocument()
    expect(screen.queryByText(/User=svdemo/)).not.toBeInTheDocument()
  })

  it('un valor en las variables no sale del panel', async () => {
    const user = userEvent.setup()
    const calls = answer()
    renderWithQuery(<NewProjectPage serverID={serverID} />)

    await fillSystemd(user)
    await user.type(screen.getByLabelText('Variables requeridas'), '=s3cr3t0')
    await user.click(screen.getByRole('button', { name: 'Ver lo que se va a escribir' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('Solo los nombres')
    expect(screen.getByLabelText('Variables requeridas')).toHaveAttribute('aria-invalid', 'true')
    expect(calls.some((c) => c.path.includes('/proposals'))).toBe(false)
    expect(document.body.textContent).not.toContain('s3cr3t0')
  })

  it('muestra el error del control plane, que nombra el campo', async () => {
    const user = userEvent.setup()
    answer({
      previewError: apiError(400, 'invalid_proposal', 'la propuesta no es válida: site.domain: tiene que ser un nombre de host'),
    })
    renderWithQuery(<NewProjectPage serverID={serverID} />)

    await fillSystemd(user)
    await user.click(screen.getByRole('button', { name: 'Ver lo que se va a escribir' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('site.domain')
    expect(screen.queryByRole('button', { name: 'Proponer' })).not.toBeInTheDocument()
  })

  it('con Docker pide el Compose y los pasos con su servicio', async () => {
    const user = userEvent.setup()
    const calls = answer()
    renderWithQuery(<NewProjectPage serverID={serverID} />)

    await user.type(await screen.findByLabelText('Nombre'), 'tienda')
    await user.type(screen.getByLabelText('Repositorio'), 'github.com/a/tienda')
    await user.click(screen.getByLabelText('Docker Compose'))
    expect(screen.queryByLabelText('Comando del servicio')).not.toBeInTheDocument()
    expect(screen.queryByLabelText('Preparar (opcional)')).not.toBeInTheDocument()
    await user.type(screen.getByLabelText('Pasos previos (opcional)'), 'web: bin/migrate')
    await user.click(screen.getByRole('button', { name: 'Ver lo que se va a escribir' }))

    await screen.findByRole('button', { name: 'Proponer' })
    const body = JSON.parse(String(calls.find((c) => c.path.endsWith('/preview'))?.init?.body))
    expect(body).toMatchObject({ runtime: 'docker', compose_file: 'docker-compose.yml', pre: [{ service: 'web', command: ['bin/migrate'] }] })
    expect(body.command).toBeUndefined()
  })
})
