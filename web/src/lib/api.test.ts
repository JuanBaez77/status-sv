import { afterEach, describe, expect, it, vi } from 'vitest'

import { ApiError, api, auth } from '@/lib/api'

function mockFetch(response: Response | Error) {
  const fetchMock = vi.fn((_path: string, _init?: RequestInit) =>
    response instanceof Error ? Promise.reject(response) : Promise.resolve(response),
  )
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('api', () => {
  it('manda la cookie de sesión y devuelve el cuerpo', async () => {
    const fetchMock = mockFetch(
      new Response(JSON.stringify({ username: 'juan' }), { status: 200 }),
    )

    await expect(api.get<{ username: string }>('/v1/auth/me')).resolves.toEqual({ username: 'juan' })

    const [path, init] = fetchMock.mock.calls[0]
    expect(path).toBe('/v1/auth/me')
    expect(init?.credentials).toBe('same-origin')
  })

  it('convierte el error de la API en un ApiError con su código', async () => {
    mockFetch(
      new Response(JSON.stringify({ error: 'invalid_credentials', message: 'usuario o contraseña incorrectos' }), {
        status: 401,
      }),
    )

    const error = await api.get('/v1/auth/me').catch((e: unknown) => e)
    expect(error).toBeInstanceOf(ApiError)
    expect(error).toMatchObject({
      status: 401,
      code: 'invalid_credentials',
      message: 'usuario o contraseña incorrectos',
    })
    expect((error as ApiError).unauthorized).toBe(true)
  })

  it('no se rompe con un error sin cuerpo JSON', async () => {
    mockFetch(new Response('502 Bad Gateway', { status: 502 }))

    const error = (await api.get('/v1/servers').catch((e: unknown) => e)) as ApiError
    expect(error.status).toBe(502)
    expect(error.code).toBe('unknown')
  })

  it('acepta una respuesta sin contenido', async () => {
    mockFetch(new Response(null, { status: 204 }))
    await expect(auth.logout()).resolves.toBeUndefined()
  })

  it('avisa cuando no se puede llegar al control plane', async () => {
    mockFetch(new TypeError('Failed to fetch'))

    const error = (await api.get('/v1/auth/me').catch((e: unknown) => e)) as ApiError
    expect(error.status).toBe(0)
    expect(error.code).toBe('network')
  })

  it('serializa el cuerpo del login', async () => {
    const fetchMock = mockFetch(new Response(JSON.stringify({ username: 'juan' }), { status: 200 }))

    await auth.login('juan', 'una contraseña larga')

    const [, init] = fetchMock.mock.calls[0]
    expect(init?.method).toBe('POST')
    expect(JSON.parse(String(init?.body))).toEqual({
      username: 'juan',
      password: 'una contraseña larga',
    })
  })
})
