import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { Session } from '@/lib/api'
import { TotpEnrollPage, TotpVerifyPage } from '@/pages/totp'
import { apiError, json, renderWithQuery, stubFetch } from '@/test/render'

const pending: Session = {
  username: 'juan',
  expires_at: '2026-09-28T12:05:00Z',
  stage: 'totp_pending',
}
const enrolling: Session = { ...pending, stage: 'totp_enroll' }
const full: Session = {
  username: 'juan',
  expires_at: '2026-10-05T12:00:00Z',
  stage: 'full',
  recovery_codes_left: 10,
}

const recoveryCodes = [
  'AAAA-BBBB-CCCC-DDDD',
  'EEEE-FFFF-GGGG-HHHH',
  'IIII-JJJJ-KKKK-LLLL',
  'MMMM-NNNN-OOOO-PPPP',
  'QQQQ-RRRR-SSSS-TTTT',
  'UUUU-VVVV-WWWW-XXXX',
  'YYYY-ZZZZ-2222-3333',
  '4444-5555-6666-7777',
  'ABCD-EFGH-IJKL-MNOP',
  'QRST-UVWX-YZ23-4567',
]

afterEach(() => {
  vi.unstubAllGlobals()
})

function bodyOf(calls: { path: string; init?: RequestInit }[], path: string) {
  const call = calls.find((c) => c.path === path)
  return call === undefined ? undefined : JSON.parse(String(call.init?.body))
}

describe('TotpVerifyPage', () => {
  it('manda el código sin espacios y pasa a la sesión completa', async () => {
    const calls = stubFetch(() => json(full))
    const user = userEvent.setup()
    const { client } = renderWithQuery(<TotpVerifyPage session={pending} />)

    await user.type(screen.getByLabelText('Código'), '123 456')
    await user.click(screen.getByRole('button', { name: 'Verificar' }))

    await waitFor(() => expect(client.getQueryData(['session'])).toEqual(full))
    expect(bodyOf(calls, '/v1/auth/totp/verify')).toEqual({ code: '123456' })
  })

  it('acepta un código de recuperación', async () => {
    const calls = stubFetch(() => json(full))
    const user = userEvent.setup()
    renderWithQuery(<TotpVerifyPage session={pending} />)

    await user.click(screen.getByRole('button', { name: /código de recuperación/ }))
    await user.type(screen.getByLabelText('Código de recuperación'), 'aaaa-bbbb-cccc-dddd')
    await user.click(screen.getByRole('button', { name: 'Verificar' }))

    await waitFor(() =>
      expect(bodyOf(calls, '/v1/auth/totp/verify')).toEqual({
        recovery_code: 'aaaa-bbbb-cccc-dddd',
      }),
    )
  })

  it('avisa si el código no es correcto', async () => {
    stubFetch(() => apiError(401, 'invalid_code', 'código incorrecto'))
    const user = userEvent.setup()
    renderWithQuery(<TotpVerifyPage session={pending} />)

    await user.type(screen.getByLabelText('Código'), '000000')
    await user.click(screen.getByRole('button', { name: 'Verificar' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('El código no es correcto')
  })

  it('cuando se cierra la verificación, lo dice y manda a poner la contraseña', async () => {
    stubFetch(() => apiError(401, 'unauthorized', 'demasiados códigos incorrectos'))
    const user = userEvent.setup()
    renderWithQuery(<TotpVerifyPage session={pending} />)

    await user.type(screen.getByLabelText('Código'), '000000')
    await user.click(screen.getByRole('button', { name: 'Verificar' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('Volvé a poner la contraseña')
  })
})

describe('TotpEnrollPage', () => {
  it('muestra el QR, confirma con un código y entrega los códigos de recuperación', async () => {
    const calls = stubFetch((path) => {
      if (path === '/v1/auth/totp/enroll') {
        return json({
          secret: 'GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ',
          otpauth_uri: 'otpauth://totp/status-sv:juan?secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ',
          qr_png: 'data:image/png;base64,iVBORw0KGgo=',
        })
      }
      return json({ ...full, recovery_codes: recoveryCodes })
    })
    const user = userEvent.setup()
    const { client } = renderWithQuery(<TotpEnrollPage session={enrolling} />)

    const qr = await screen.findByRole('img', { name: /Código QR/ })
    expect(qr).toHaveAttribute('src', 'data:image/png;base64,iVBORw0KGgo=')
    // La clave para cargar a mano va en grupos de cuatro.
    expect(screen.getByText('GEZD GNBV GY3T QOJQ GEZD GNBV GY3T QOJQ')).toBeInTheDocument()

    await user.type(screen.getByLabelText('Código de la app'), '123456')
    await user.click(screen.getByRole('button', { name: 'Activar' }))

    const list = await screen.findByRole('list', {
      name: 'Códigos de recuperación',
    })
    expect(list.querySelectorAll('li')).toHaveLength(10)
    expect(bodyOf(calls, '/v1/auth/totp/confirm')).toEqual({ code: '123456' })
    // Hasta que no se confirma que se guardaron, no se entra al panel.
    expect(client.getQueryData(['session'])).toBeUndefined()

    await user.click(screen.getByRole('button', { name: /Ya los guardé/ }))
    await waitFor(() =>
      expect(client.getQueryData(['session'])).toEqual({
        ...full,
        recovery_codes: recoveryCodes,
      }),
    )
    expect(calls.filter((call) => call.path === '/v1/auth/totp/enroll')).toHaveLength(1)
  })

  it('avisa si el código de confirmación no es correcto', async () => {
    stubFetch((path) =>
      path === '/v1/auth/totp/enroll'
        ? json({
            secret: 'GEZDGNBV',
            otpauth_uri: 'otpauth://totp/x',
            qr_png: 'data:image/png;base64,',
          })
        : apiError(401, 'invalid_code', 'código incorrecto'),
    )
    const user = userEvent.setup()
    renderWithQuery(<TotpEnrollPage session={enrolling} />)

    await user.type(await screen.findByLabelText('Código de la app'), '000000')
    await user.click(screen.getByRole('button', { name: 'Activar' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('El código no es correcto')
  })
})
