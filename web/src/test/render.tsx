import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render } from '@testing-library/react'
import type { ReactElement } from 'react'
import { vi } from 'vitest'

/** renderWithQuery monta un componente con su propio cliente de datos. */
export function renderWithQuery(ui: ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return { client, ...render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>) }
}

type Handler = (path: string, init?: RequestInit) => Response | Promise<Response>

/**
 * stubFetch reemplaza fetch por una función que responde según la ruta, y
 * devuelve las llamadas que se hicieron.
 */
export function stubFetch(handler: Handler) {
  const calls: { path: string; init?: RequestInit }[] = []
  const mock = vi.fn(async (path: string, init?: RequestInit) => {
    calls.push({ path, init })
    return handler(path, init)
  })
  vi.stubGlobal('fetch', mock)
  return calls
}

/** json arma una respuesta con cuerpo JSON. */
export function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status })
}

/** apiError arma la respuesta de error que devuelve el control plane. */
export function apiError(status: number, code: string, message = 'error') {
  return json({ error: code, message }, status)
}
