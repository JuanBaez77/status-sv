import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { LoginPage } from '@/pages/login'
import { apiError, json, renderWithQuery, stubFetch } from '@/test/render'

afterEach(() => {
  vi.unstubAllGlobals()
})

async function fillAndSubmit(user: ReturnType<typeof userEvent.setup>) {
  await user.type(screen.getByLabelText('Usuario'), 'juan')
  await user.type(screen.getByLabelText('Contraseña'), 'una contraseña larga')
  await user.click(screen.getByRole('button', { name: 'Entrar' }))
}

describe('LoginPage', () => {
  it('manda usuario y contraseña, y guarda la sesión', async () => {
    const calls = stubFetch(() => json({ username: 'juan', expires_at: '2026-09-29T12:00:00Z' }))
    const user = userEvent.setup()
    const { client } = renderWithQuery(<LoginPage />)

    await fillAndSubmit(user)

    await waitFor(() => {
      expect(client.getQueryData(['session'])).toEqual({
        username: 'juan',
        expires_at: '2026-09-29T12:00:00Z',
      })
    })

    const login = calls.find((call) => call.path === '/v1/auth/login')
    expect(JSON.parse(String(login?.init?.body))).toEqual({
      username: 'juan',
      password: 'una contraseña larga',
    })
  })

  it('la contraseña no queda en el formulario después de entrar', async () => {
    stubFetch(() => json({ username: 'juan', expires_at: '2026-09-29T12:00:00Z' }))
    const user = userEvent.setup()
    renderWithQuery(<LoginPage />)

    await fillAndSubmit(user)

    await waitFor(() => {
      expect(screen.getByLabelText('Contraseña')).toHaveValue('')
    })
  })

  it('muestra el error de credenciales sin decir qué falló', async () => {
    stubFetch(() => apiError(401, 'invalid_credentials', 'usuario o contraseña incorrectos'))
    const user = userEvent.setup()
    renderWithQuery(<LoginPage />)

    await fillAndSubmit(user)

    expect(await screen.findByRole('alert')).toHaveTextContent('Usuario o contraseña incorrectos')
  })

  it('avisa cuando el control plane frenó los intentos', async () => {
    stubFetch(() => apiError(429, 'rate_limited'))
    const user = userEvent.setup()
    renderWithQuery(<LoginPage />)

    await fillAndSubmit(user)

    expect(await screen.findByRole('alert')).toHaveTextContent('Esperá un minuto')
  })

  it('no deja mandar dos veces mientras espera respuesta', async () => {
    let resolve: (value: Response) => void = () => {}
    const pending = new Promise<Response>((r) => {
      resolve = r
    })
    const calls = stubFetch(() => pending)
    const user = userEvent.setup()
    renderWithQuery(<LoginPage />)

    await fillAndSubmit(user)

    const button = await screen.findByRole('button', { name: 'Entrando…' })
    expect(button).toBeDisabled()
    await user.click(button)
    expect(calls.filter((call) => call.path === '/v1/auth/login')).toHaveLength(1)

    resolve(json({ username: 'juan', expires_at: '2026-09-29T12:00:00Z' }))
  })
})
