import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Router } from 'wouter'
import { memoryLocation } from 'wouter/memory-location'

import type { AuditEntry } from '@/lib/api'
import { AuditPage } from '@/pages/audit'
import { apiError, json, renderWithQuery, stubFetch } from '@/test/render'

function entry(id: string, overrides: Partial<AuditEntry> = {}): AuditEntry {
  return {
    id,
    at: '2026-10-02T06:10:00Z',
    actor: 'juan',
    actor_type: 'user',
    action: 'auth.login',
    target_type: 'user',
    target_id: 'juan',
    result: 'ok',
    ip: '203.0.113.7',
    detail: {},
    ...overrides,
  }
}

/** renderAt monta la página en una dirección, con el historial en memoria. */
function renderAt(path: string) {
  const location = memoryLocation({ path, record: true })
  const view = renderWithQuery(
    <Router hook={location.hook} searchHook={location.searchHook}>
      <AuditPage />
    </Router>,
  )
  return { ...view, location }
}

function queryOf(path: string): URLSearchParams {
  return new URLSearchParams(path.split('?')[1] ?? '')
}

beforeEach(() => {
  vi.useRealTimers()
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('AuditPage', () => {
  it('muestra quién hizo qué, sobre qué y cómo salió', async () => {
    stubFetch(() =>
      json({
        entries: [
          entry('a', {
            action: 'deploy.finish',
            actor: 'sv-gcg',
            actor_type: 'agent',
            target_type: 'project',
            target_id: 'id-del-proyecto',
            result: 'rolled_back',
            ip: '',
            detail: { proyecto: 'gcg-alertas', codigo: 'unhealthy' },
          }),
          entry('b', { result: 'failed', actor: 'desconocido', target_id: 'desconocido' }),
        ],
      }),
    )

    renderAt('/audit')

    const rows = await screen.findAllByRole('listitem')
    expect(rows).toHaveLength(2)
    expect(within(rows[0]).getByText('sv-gcg')).toBeInTheDocument()
    expect(within(rows[0]).getByText('(Agent)')).toBeInTheDocument()
    expect(within(rows[0]).getByText(/Terminó un deploy/)).toBeInTheDocument()
    // El objetivo sale por el nombre del proyecto, no por su identificador.
    expect(within(rows[0]).getByText('gcg-alertas')).toBeInTheDocument()
    expect(within(rows[0]).getByText('Volvió atrás')).toBeInTheDocument()
    expect(within(rows[1]).getByText('Falló')).toBeInTheDocument()
    expect(within(rows[1]).getByText(/203\.0\.113\.7/)).toBeInTheDocument()
    expect(screen.getByText('No hay más renglones.')).toBeInTheDocument()
  })

  it('el detalle de un renglón se abre a pedido', async () => {
    stubFetch(() =>
      json({ entries: [entry('a', { action: 'logs.request', detail: { renglones: 200 } })] }),
    )
    const user = userEvent.setup()

    renderAt('/audit')

    const row = await screen.findByRole('button', { name: /Pidió logs/ })
    expect(screen.queryByText('logs.request')).not.toBeInTheDocument()
    await user.click(row)
    expect(row).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByText('logs.request')).toBeInTheDocument()
    expect(screen.getByText('renglones')).toBeInTheDocument()
    expect(screen.getByText('200')).toBeInTheDocument()
  })

  it('trae la página siguiente con el cursor de la anterior', async () => {
    const calls = stubFetch((path) =>
      queryOf(path).get('before') === 'cursor-1'
        ? json({ entries: [entry('c')] })
        : json({ entries: [entry('a'), entry('b')], next_cursor: 'cursor-1' }),
    )
    const user = userEvent.setup()

    renderAt('/audit')

    await user.click(await screen.findByRole('button', { name: 'Cargar más' }))
    expect(await screen.findByText('No hay más renglones.')).toBeInTheDocument()
    expect(screen.getAllByRole('listitem')).toHaveLength(3)
    expect(calls.map((c) => queryOf(c.path).get('before'))).toEqual([null, 'cursor-1'])
  })

  it('filtrar cambia la dirección y vuelve a pedir desde el principio', async () => {
    const calls = stubFetch(() => json({ entries: [entry('a')] }))
    const user = userEvent.setup()

    const { location } = renderAt('/audit')

    await screen.findAllByRole('listitem')
    await user.selectOptions(screen.getByLabelText('Acción'), 'auth.login')
    await user.selectOptions(screen.getByLabelText('Resultado'), 'failed')

    await vi.waitFor(() => {
      const last = queryOf(calls[calls.length - 1].path)
      expect(last.get('action')).toBe('auth.login')
      expect(last.get('result')).toBe('failed')
      expect(last.get('before')).toBeNull()
    })
    expect(location.history?.at(-1)).toBe('/audit?action=auth.login&result=failed')
  })

  it('un enlace desde un proyecto llega con el filtro puesto, y se puede quitar', async () => {
    const calls = stubFetch(() => json({ entries: [] }))
    const user = userEvent.setup()

    renderAt('/audit?target_type=project&target_id=id-del-proyecto')

    expect(await screen.findByText('Nada coincide con esos filtros')).toBeInTheDocument()
    expect(screen.getByText('id-del-proyecto')).toBeInTheDocument()
    expect(queryOf(calls[0].path).get('target_id')).toBe('id-del-proyecto')
    expect(queryOf(calls[0].path).get('target_type')).toBe('project')

    await user.click(screen.getByRole('button', { name: 'Quitar los filtros' }))
    expect(await screen.findByText('Todavía no hay nada anotado')).toBeInTheDocument()
    expect(calls[calls.length - 1].path).toBe('/v1/audit')
  })

  it('si la API falla, lo dice', async () => {
    stubFetch(() => apiError(500, 'internal', 'error interno'))

    renderAt('/audit')

    expect(await screen.findByText('No se pudo traer la auditoría')).toBeInTheDocument()
    expect(screen.getByText('error interno')).toBeInTheDocument()
  })
})
