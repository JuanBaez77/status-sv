import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { DeploysCard } from '@/components/deploy-panel'
import type { DeployDetail, DeploySummary, ProjectDetail } from '@/lib/api'
import { panelSocket } from '@/lib/panel-socket'
import { apiError, json, renderWithQuery, stubFetch } from '@/test/render'
import { FakeWebSocket } from '@/test/websocket'

const shaOld = '1111111111111111111111111111111111111111'
const shaNew = '2222222222222222222222222222222222222222'

function project(overrides: Partial<ProjectDetail> = {}): ProjectDetail {
  return {
    id: 'p1',
    name: 'tienda',
    server_id: 's1',
    server_name: 'vps-cristales',
    environment: 'staging',
    runtime: 'docker',
    repo: 'github.com/x/tienda',
    branch: 'main',
    status: 'ok',
    units: { total: 1, healthy: 1 },
    health: null,
    seen_at: '2026-09-30T12:00:00Z',
    workdir: '/srv/tienda',
    compose_file: 'compose.yml',
    compose_project: 'tienda',
    missing_units: [],
    containers: [],
    services: [],
    source: null,
    ...overrides,
  }
}

function summary(overrides: Partial<DeploySummary> = {}): DeploySummary {
  return {
    id: 'd1',
    kind: 'deploy',
    commit: shaNew,
    previous_commit: shaOld,
    status: 'succeeded',
    requested_by: 'juan',
    requested_at: '2026-09-30T12:00:00Z',
    started_at: '2026-09-30T12:00:01Z',
    finished_at: '2026-09-30T12:01:31Z',
    ...overrides,
  }
}

function detail(overrides: Partial<DeployDetail> = {}): DeployDetail {
  return {
    ...summary(),
    project_id: 'p1',
    project_name: 'tienda',
    server_id: 's1',
    server_name: 'vps-cristales',
    environment: 'staging',
    steps: [],
    ...overrides,
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

describe('DeploysCard', () => {
  it('pide el nombre del proyecto antes de desplegar, y nada más en staging', async () => {
    const calls = stubFetch((path, init) => {
      if (path === '/v1/projects/p1/deploys' && init?.method === 'POST') {
        return json(detail({ id: 'd2', status: 'pending', steps: [] }), 201)
      }
      if (path === '/v1/projects/p1/deploys') return json({ deploys: [summary()] })
      return json(detail({ id: 'd2', status: 'pending' }))
    })
    renderWithQuery(<DeploysCard project={project()} />)

    const history = await screen.findByRole('list', { name: 'Historia de deploys' })
    expect(within(history).getByText('Desplegado')).toBeInTheDocument()
    expect(within(history).getByText('22222222')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: /Desplegar/ }))
    const form = screen.getByRole('form', { name: 'Desplegar' })
    const submit = within(form).getByRole('button', { name: 'Desplegar' })
    expect(submit).toBeDisabled()
    expect(within(form).queryByLabelText(/Código de la app/)).not.toBeInTheDocument()

    fireEvent.change(within(form).getByLabelText(/Para confirmar/), { target: { value: 'tiend' } })
    expect(submit).toBeDisabled()
    fireEvent.change(within(form).getByLabelText(/Commit/), { target: { value: 'main' } })
    fireEvent.change(within(form).getByLabelText(/Para confirmar/), { target: { value: 'tienda' } })
    // Un commit que no es un SHA completo no se manda.
    expect(submit).toBeDisabled()
    fireEvent.change(within(form).getByLabelText(/Commit/), { target: { value: shaNew } })
    expect(submit).toBeEnabled()
    fireEvent.click(submit)

    await waitFor(() => {
      const post = calls.find((c) => c.init?.method === 'POST')
      expect(post?.path).toBe('/v1/projects/p1/deploys')
      expect(JSON.parse(post?.init?.body as string)).toEqual({ confirm: 'tienda', commit: shaNew })
    })
  })

  it('en producción pide además un código de la app, y explica si no sirve', async () => {
    const calls = stubFetch((path, init) => {
      if (init?.method === 'POST') return apiError(403, 'invalid_code', 'código incorrecto')
      if (path === '/v1/projects/p1/deploys') return json({ deploys: [] })
      return json(detail())
    })
    renderWithQuery(<DeploysCard project={project({ environment: 'production' })} />)

    await screen.findByText('Todavía no se desplegó desde el panel.')
    fireEvent.click(screen.getByRole('button', { name: /Desplegar/ }))
    const form = screen.getByRole('form', { name: 'Desplegar' })
    expect(within(form).getByText('(producción)')).toBeInTheDocument()
    fireEvent.change(within(form).getByLabelText(/Para confirmar/), { target: { value: 'tienda' } })
    const submit = within(form).getByRole('button', { name: 'Desplegar' })
    expect(submit).toBeDisabled()

    fireEvent.change(within(form).getByLabelText(/Código de la app/), {
      target: { value: '12a34 56' },
    })
    expect(within(form).getByLabelText(/Código de la app/)).toHaveValue('123456')
    fireEvent.click(submit)

    expect(await within(form).findByRole('alert')).toHaveTextContent(/uno nuevo de la app/)
    const post = calls.find((c) => c.init?.method === 'POST')
    expect(JSON.parse(post?.init?.body as string)).toEqual({
      confirm: 'tienda',
      totp_code: '123456',
    })
  })

  it('muestra en vivo los pasos del deploy en curso', async () => {
    stubFetch((path) => {
      if (path === '/v1/projects/p1/deploys') {
        return json({ deploys: [summary({ id: 'd3', status: 'running', finished_at: undefined })] })
      }
      return json(
        detail({
          id: 'd3',
          status: 'running',
          finished_at: undefined,
          steps: [
            {
              name: 'preflight',
              status: 'ok',
              started_at: '2026-09-30T12:00:01Z',
              finished_at: '2026-09-30T12:00:02Z',
            },
            { name: 'pre', service: 'backend', status: 'ok', detail: 'python manage.py migrate' },
            {
              name: 'build',
              status: 'running',
              started_at: '2026-09-30T12:00:02Z',
              output: ['#5 RUN pip install'],
            },
          ],
        }),
      )
    })
    renderWithQuery(<DeploysCard project={project()} />)

    expect(await screen.findByText('Compilar')).toBeInTheDocument()
    expect(screen.getByText('Paso previo (backend)')).toBeInTheDocument()
    // La salida del paso en curso se ve sin tocar nada.
    expect(screen.getByText('#5 RUN pip install')).toBeInTheDocument()
    expect(screen.getByText('11111111 → 22222222')).toBeInTheDocument()
    // Mientras hay uno en curso no se puede pedir otro.
    expect(screen.getByRole('button', { name: /Desplegar/ })).toBeDisabled()
    expect(screen.getByRole('button', { name: /Volver atrás/ })).toBeDisabled()
  })

  it('explica por qué falló un deploy de la historia', async () => {
    stubFetch(() =>
      json({
        deploys: [
          summary({
            status: 'failed',
            code: 'rollback_failed',
            message: 'lo anterior tampoco levantó',
          }),
        ],
      }),
    )
    renderWithQuery(<DeploysCard project={project()} />)

    expect(await screen.findByText('Falló')).toBeInTheDocument()
    expect(screen.getByText(/hay que mirarlo/)).toBeInTheDocument()
  })

  it('el rollback avisa que la base no vuelve atrás y no pide commit', async () => {
    stubFetch(() => json({ deploys: [summary()] }))
    renderWithQuery(<DeploysCard project={project()} />)

    await screen.findByText('Desplegado')
    fireEvent.click(screen.getByRole('button', { name: /Volver atrás/ }))
    const form = screen.getByRole('form', { name: 'Volver atrás' })
    expect(within(form).getByText(/no se deshacen/)).toBeInTheDocument()
    expect(within(form).queryByLabelText(/Commit/)).not.toBeInTheDocument()
  })

  describe('con un proyecto de systemd', () => {
    const facturas = () =>
      project({
        name: 'facturas',
        runtime: 'systemd',
        compose_file: undefined,
        compose_project: undefined,
        declared_units: ['gunicorn-facturas.service', 'facturas-worker.service'],
      })

    it('se despliega igual, y dice qué servicios reinicia', async () => {
      stubFetch(() => json({ deploys: [] }))
      renderWithQuery(<DeploysCard project={facturas()} />)

      expect(
        await screen.findByText(/reinicia gunicorn-facturas.service y facturas-worker.service/),
      ).toBeInTheDocument()
      expect(screen.getByRole('button', { name: /Desplegar/ })).toBeEnabled()
      expect(screen.getByRole('button', { name: /Volver atrás/ })).toBeEnabled()
    })

    it('muestra sus pasos: preparar con su comando y reiniciar', async () => {
      stubFetch((path) => {
        if (path === '/v1/projects/p1/deploys') {
          return json({
            deploys: [summary({ id: 'd4', status: 'running', finished_at: undefined })],
          })
        }
        return json(
          detail({
            id: 'd4',
            status: 'running',
            finished_at: undefined,
            steps: [
              { name: 'build', status: 'ok', detail: '.venv/bin/pip install -r requirements.txt' },
              { name: 'pre', status: 'ok', detail: '.venv/bin/python manage.py migrate' },
              {
                name: 'restart',
                status: 'running',
                output: ['reiniciando gunicorn-facturas.service'],
              },
            ],
          }),
        )
      })
      renderWithQuery(<DeploysCard project={facturas()} />)

      // No compila nada: prepara el código, y el comando va al lado.
      expect(await screen.findByText('Preparar')).toBeInTheDocument()
      expect(screen.queryByText('Compilar')).not.toBeInTheDocument()
      expect(screen.getByText('.venv/bin/pip install -r requirements.txt')).toBeInTheDocument()
      // Un paso declarado de systemd no lleva servicio de Compose.
      expect(screen.getByText('Paso previo')).toBeInTheDocument()
      expect(screen.getByText('Reiniciar')).toBeInTheDocument()
      expect(screen.getByText('reiniciando gunicorn-facturas.service')).toBeInTheDocument()
    })

    it('el rollback avisa que corre de nuevo lo que prepara el código', async () => {
      stubFetch(() => json({ deploys: [summary()] }))
      renderWithQuery(<DeploysCard project={facturas()} />)

      await screen.findByText('Desplegado')
      fireEvent.click(screen.getByRole('button', { name: /Volver atrás/ }))
      const form = screen.getByRole('form', { name: 'Volver atrás' })
      expect(
        within(form).getByText(/se\s+corren de nuevo los pasos que lo preparan/),
      ).toBeInTheDocument()
      expect(within(form).queryByText(/sin compilar/)).not.toBeInTheDocument()
      expect(within(form).getByText(/no se deshacen/)).toBeInTheDocument()
    })
  })
  describe('con un proyecto que todavía no corre en el servidor', () => {
    const nuevo = (overrides: Partial<ProjectDetail> = {}) =>
      project({ status: 'down', units: { total: 0, healthy: 0 }, ...overrides })

    it('ofrece el primer deploy, sin commit y con el código de la app aunque sea staging', async () => {
      const calls = stubFetch((path, init) => {
        if (path === '/v1/projects/p1/bootstrap' && init?.method === 'POST') {
          return json(detail({ id: 'd9', kind: 'bootstrap', status: 'pending' }), 201)
        }
        if (path === '/v1/projects/p1/deploys') return json({ deploys: [] })
        return json(detail({ id: 'd9', kind: 'bootstrap', status: 'pending' }))
      })
      renderWithQuery(<DeploysCard project={nuevo()} />)

      expect(await screen.findByText(/todavía no corre en el servidor/)).toBeInTheDocument()
      // Desplegar sigue estando: el proyecto pudo haberse clonado a mano.
      expect(screen.getByRole('button', { name: /Desplegar/ })).toBeEnabled()
      fireEvent.click(screen.getByRole('button', { name: /Primer deploy/ }))

      const form = screen.getByRole('form', { name: 'Primer deploy' })
      expect(within(form).getByText(/baja lo que levantó/)).toBeInTheDocument()
      expect(within(form).getByText('/srv/tienda')).toBeInTheDocument()
      expect(within(form).queryByLabelText(/Commit/)).not.toBeInTheDocument()

      const submit = within(form).getByRole('button', { name: 'Primer deploy' })
      fireEvent.change(within(form).getByLabelText(/Para confirmar/), {
        target: { value: 'tienda' },
      })
      // Con el nombre solo no alcanza: el primer deploy pide el código siempre.
      expect(submit).toBeDisabled()
      expect(within(form).getByText(/lo pide siempre/)).toBeInTheDocument()
      fireEvent.change(within(form).getByLabelText(/Código de la app/), {
        target: { value: '654321' },
      })
      expect(submit).toBeEnabled()
      fireEvent.click(submit)

      await waitFor(() => {
        const post = calls.find((c) => c.init?.method === 'POST')
        expect(post?.path).toBe('/v1/projects/p1/bootstrap')
        expect(JSON.parse(post?.init?.body as string)).toEqual({
          confirm: 'tienda',
          totp_code: '654321',
        })
      })
    })

    it('no lo ofrece si algo del proyecto corre o si ya hubo un deploy bueno', async () => {
      stubFetch(() => json({ deploys: [] }))
      const { unmount } = renderWithQuery(<DeploysCard project={project()} />)
      await screen.findByText('Todavía no se desplegó desde el panel.')
      expect(screen.queryByRole('button', { name: /Primer deploy/ })).not.toBeInTheDocument()
      unmount()

      stubFetch(() => json({ deploys: [summary()] }))
      renderWithQuery(<DeploysCard project={nuevo()} />)
      await screen.findByText('Desplegado')
      expect(screen.queryByRole('button', { name: /Primer deploy/ })).not.toBeInTheDocument()
    })

    it('muestra sus pasos con otro nombre, y que se bajó si no quedó bien', async () => {
      stubFetch((path) => {
        if (path === '/v1/projects/p1/deploys') {
          return json({
            deploys: [
              summary({
                id: 'd8',
                kind: 'bootstrap',
                status: 'rolled_back',
                code: 'unhealthy',
                previous_commit: undefined,
              }),
            ],
          })
        }
        return json(
          detail({
            id: 'd8',
            kind: 'bootstrap',
            status: 'rolled_back',
            code: 'unhealthy',
            previous_commit: undefined,
            steps: [
              { name: 'fetch', status: 'ok' },
              { name: 'checkout', status: 'ok' },
              { name: 'up', status: 'ok' },
              { name: 'healthcheck', status: 'failed' },
              { name: 'rollback', status: 'ok', detail: 'bajó lo que había levantado' },
            ],
          }),
        )
      })
      renderWithQuery(<DeploysCard project={nuevo()} />)

      const history = await screen.findByRole('list', { name: 'Historia de deploys' })
      expect(within(history).getByText('Se bajó')).toBeInTheDocument()
      expect(within(history).getByText('primer deploy')).toBeInTheDocument()
      // No quedó nada corriendo: se puede intentar de nuevo.
      expect(screen.getByRole('button', { name: /Primer deploy/ })).toBeEnabled()

      fireEvent.click(within(history).getByRole('button', { name: 'Pasos' }))
      expect(await screen.findByText('Clonar la rama')).toBeInTheDocument()
      expect(screen.getByText('Dejar el código')).toBeInTheDocument()
      expect(screen.getByText('Bajar lo levantado')).toBeInTheDocument()
      expect(screen.queryByText('Volver atrás', { selector: 'span' })).not.toBeInTheDocument()
    })
  })
})
