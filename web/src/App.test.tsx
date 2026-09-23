import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ReactElement } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import App from '@/App'

/** renderApp monta la aplicación con su propio cliente de datos. */
function renderApp(ui: ReactElement = <App />) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  return render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>)
}

function respondTo(handler: (path: string) => Response) {
  vi.stubGlobal(
    'fetch',
    vi.fn((path: string) => Promise.resolve(handler(path))),
  )
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('App', () => {
  it('sin sesión pide iniciar sesión', async () => {
    respondTo(() => new Response(JSON.stringify({ error: 'unauthorized' }), { status: 401 }))

    renderApp()

    expect(await screen.findByText('Iniciá sesión')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /salir/i })).not.toBeInTheDocument()
  })

  it('con sesión muestra el marco del panel y el nombre del usuario', async () => {
    respondTo(
      () =>
        new Response(JSON.stringify({ username: 'juan', expires_at: '2026-09-29T12:00:00Z' }), {
          status: 200,
        }),
    )

    renderApp()

    expect(await screen.findByText('juan')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /salir/i })).toBeInTheDocument()
    expect(screen.getByText('Servidores')).toBeInTheDocument()
  })

  it('avisa cuando el control plane no responde', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))),
    )

    renderApp()

    expect(await screen.findByText(/No se puede hablar con el control plane/)).toBeInTheDocument()
  })

  it('el botón de salir cierra la sesión en el servidor', async () => {
    const calls: string[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn((path: string) => {
        calls.push(path)
        if (path === '/v1/auth/logout') {
          return Promise.resolve(new Response(null, { status: 204 }))
        }
        return Promise.resolve(
          new Response(JSON.stringify({ username: 'juan', expires_at: '2026-09-29T12:00:00Z' }), {
            status: 200,
          }),
        )
      }),
    )

    renderApp()
    await userEvent.click(await screen.findByRole('button', { name: /salir/i }))

    expect(calls).toContain('/v1/auth/logout')
  })
})
