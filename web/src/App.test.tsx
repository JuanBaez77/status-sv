import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import App from '@/App'
import { apiError, json, renderWithQuery, stubFetch } from '@/test/render'

const session = { username: 'juan', expires_at: '2026-09-29T12:00:00Z' }

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('App', () => {
  it('sin sesión muestra el login y nada del panel', async () => {
    stubFetch(() => apiError(401, 'unauthorized', 'iniciá sesión'))

    renderWithQuery(<App />)

    expect(await screen.findByLabelText('Usuario')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /salir/i })).not.toBeInTheDocument()
  })

  it('con sesión muestra el marco del panel y la lista de servidores', async () => {
    stubFetch((path) => (path === '/v1/servers' ? json({ servers: [] }) : json(session)))

    renderWithQuery(<App />)

    expect(await screen.findByText('juan')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /salir/i })).toBeInTheDocument()
    expect(await screen.findByText('Todavía no hay servidores')).toBeInTheDocument()
  })

  it('avisa cuando el control plane no responde', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))),
    )

    renderWithQuery(<App />)

    expect(await screen.findByText(/No se puede hablar con el control plane/)).toBeInTheDocument()
  })

  it('el botón de salir cierra la sesión en el servidor', async () => {
    const calls = stubFetch((path) => {
      if (path === '/v1/auth/logout') return new Response(null, { status: 204 })
      if (path === '/v1/servers') return json({ servers: [] })
      return json(session)
    })

    renderWithQuery(<App />)
    await userEvent.click(await screen.findByRole('button', { name: /salir/i }))

    expect(calls.map((call) => call.path)).toContain('/v1/auth/logout')
  })
})
