import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useMemo, useState, type FormEvent, type ReactNode } from 'react'

import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { sessionKey, useLogout } from '@/hooks/use-session'
import { ApiError, auth, type Session, type TotpConfirmation, type TotpEnrollment } from '@/lib/api'

/**
 * Pantallas del segundo factor (ADR-013). Con la contraseña correcta el control
 * plane abre una sesión a medias; estas pantallas son lo único que sirve con
 * ella: pedir el código de la app, o darla de alta si todavía no hay.
 */

/** TotpVerifyPage pide el código de la app, o uno de recuperación. */
export function TotpVerifyPage({ session }: { session: Session }) {
  const [useRecovery, setUseRecovery] = useState(false)
  const [code, setCode] = useState('')
  const finish = useFinishLogin()

  const verify = useMutation({
    mutationFn: () =>
      auth.totpVerify(useRecovery ? { recovery_code: code } : { code: code.replace(/\s/g, '') }),
    onSuccess: finish.done,
    onError: finish.failed,
  })

  function onSubmit(event: FormEvent) {
    event.preventDefault()
    verify.mutate()
  }

  return (
    <Frame
      title="Código de verificación"
      description={
        useRecovery
          ? `Escribí uno de los códigos de recuperación de ${session.username}. Cada uno sirve una sola vez.`
          : `Abrí la app de autenticación y escribí el código de status-sv para ${session.username}.`
      }
    >
      <form onSubmit={onSubmit} className="flex flex-col gap-4">
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="code">{useRecovery ? 'Código de recuperación' : 'Código'}</Label>
          <Input
            id="code"
            name="code"
            autoComplete="one-time-code"
            inputMode={useRecovery ? 'text' : 'numeric'}
            autoFocus
            required
            placeholder={useRecovery ? 'XXXX-XXXX-XXXX-XXXX' : '123456'}
            value={code}
            onChange={(event) => setCode(event.target.value)}
          />
        </div>

        {verify.isError && <ErrorMessage error={verify.error} />}

        <Button type="submit" disabled={verify.isPending}>
          {verify.isPending ? 'Verificando…' : 'Verificar'}
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={() => {
            setUseRecovery(!useRecovery)
            setCode('')
            verify.reset()
          }}
        >
          {useRecovery
            ? 'Usar el código de la app'
            : 'No tengo el teléfono: usar un código de recuperación'}
        </Button>
        <BackToLogin />
      </form>
    </Frame>
  )
}

/**
 * TotpEnrollPage da de alta el segundo factor: muestra el QR, pide un código
 * para confirmar y, al final, los códigos de recuperación. La sesión completa
 * recién se usa cuando la persona dice que los guardó.
 */
export function TotpEnrollPage({ session }: { session: Session }) {
  const [code, setCode] = useState('')
  const [confirmed, setConfirmed] = useState<TotpConfirmation | null>(null)
  const finish = useFinishLogin()

  // El alta se pide una sola vez por pantalla. Pedirla de nuevo genera otro
  // secreto y descarta el anterior: si llegaran dos respuestas en desorden, el
  // QR no coincidiría con el secreto guardado. Como query, se deduplica (también
  // con el doble montaje de StrictMode) y no se repite sola.
  const enroll = useQuery<TotpEnrollment, ApiError>({
    queryKey: ['totp-enroll'],
    queryFn: auth.totpEnroll,
    staleTime: Infinity,
    gcTime: 0,
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })
  const { failed } = finish
  useEffect(() => {
    if (enroll.error) failed(enroll.error)
  }, [enroll.error, failed])

  const confirm = useMutation({
    mutationFn: () => auth.totpConfirm(code.replace(/\s/g, '')),
    onSuccess: (result) => {
      setCode('')
      setConfirmed(result)
    },
    onError: finish.failed,
  })

  if (confirmed !== null) {
    return <RecoveryCodes codes={confirmed.recovery_codes} onDone={() => finish.done(confirmed)} />
  }

  function onSubmit(event: FormEvent) {
    event.preventDefault()
    confirm.mutate()
  }

  return (
    <Frame
      title="Activá el segundo factor"
      description={`Para entrar al panel, ${session.username} necesita una app de autenticación (Google Authenticator, 1Password, Aegis…).`}
    >
      <div className="flex flex-col gap-4">
        {enroll.isPending && (
          <p className="text-sm text-muted-foreground">Generando el código QR…</p>
        )}

        {enroll.data && (
          <>
            <ol className="list-decimal space-y-1 pl-4 text-sm text-muted-foreground">
              <li>Escaneá el QR con la app.</li>
              <li>Escribí el código de seis dígitos que muestra.</li>
            </ol>
            <img
              src={enroll.data.qr_png}
              alt="Código QR para la app de autenticación"
              className="mx-auto size-48 rounded-md bg-white p-2 [image-rendering:pixelated]"
            />
            <details className="text-sm">
              <summary className="cursor-pointer text-muted-foreground">
                ¿No podés escanearlo?
              </summary>
              <p className="mt-2 text-muted-foreground">Cargá esta clave a mano en la app:</p>
              <code className="mt-1 block break-all rounded-md bg-muted px-2 py-1 font-mono text-xs">
                {groupsOf4(enroll.data.secret)}
              </code>
            </details>

            <form onSubmit={onSubmit} className="flex flex-col gap-4">
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="code">Código de la app</Label>
                <Input
                  id="code"
                  name="code"
                  autoComplete="one-time-code"
                  inputMode="numeric"
                  required
                  placeholder="123456"
                  value={code}
                  onChange={(event) => setCode(event.target.value)}
                />
              </div>
              {confirm.isError && <ErrorMessage error={confirm.error} />}
              <Button type="submit" disabled={confirm.isPending}>
                {confirm.isPending ? 'Confirmando…' : 'Activar'}
              </Button>
            </form>
          </>
        )}

        {enroll.isError && <ErrorMessage error={enroll.error} />}
        <BackToLogin />
      </div>
    </Frame>
  )
}

/** RecoveryCodes muestra los códigos de recuperación, que no se vuelven a ver. */
function RecoveryCodes({ codes, onDone }: { codes: string[]; onDone: () => void }) {
  const [copied, setCopied] = useState(false)

  async function copy() {
    try {
      await navigator.clipboard.writeText(codes.join('\n'))
      setCopied(true)
    } catch {
      setCopied(false)
    }
  }

  return (
    <Frame
      title="Guardá los códigos de recuperación"
      description="Si perdés el teléfono, cada uno de estos códigos te deja entrar una vez. No se vuelven a mostrar: guardalos fuera del teléfono, por ejemplo en tu gestor de contraseñas."
    >
      <div className="flex flex-col gap-4">
        <ul
          aria-label="Códigos de recuperación"
          className="grid grid-cols-2 gap-x-4 gap-y-1 rounded-md bg-muted p-3 font-mono text-sm"
        >
          {codes.map((code) => (
            <li key={code}>{code}</li>
          ))}
        </ul>
        <Button type="button" variant="secondary" onClick={copy}>
          {copied ? 'Copiados' : 'Copiar los códigos'}
        </Button>
        <Button type="button" onClick={onDone}>
          Ya los guardé: entrar al panel
        </Button>
      </div>
    </Frame>
  )
}

/**
 * useFinishLogin cierra el paso del segundo factor: con éxito, la sesión nueva
 * pasa a ser la del panel; si el control plane cerró la sesión a medias (se
 * venció o hubo demasiados intentos), se vuelve al login.
 */
function useFinishLogin() {
  const client = useQueryClient()
  return useMemo(
    () => ({
      done: (session: Session) => {
        client.setQueryData(sessionKey, session)
      },
      failed: (error: unknown) => {
        if (error instanceof ApiError && error.code === 'unauthorized') {
          // Se deja ver el mensaje un momento; después /me contesta 401 y App
          // muestra el login.
          window.setTimeout(() => void client.invalidateQueries({ queryKey: sessionKey }), 1500)
        }
      },
    }),
    [client],
  )
}

function BackToLogin() {
  const logout = useLogout()
  return (
    <Button
      type="button"
      variant="ghost"
      size="sm"
      onClick={() => logout.mutate()}
      disabled={logout.isPending}
    >
      Volver a la contraseña
    </Button>
  )
}

function ErrorMessage({ error }: { error: unknown }) {
  return (
    <p role="alert" className="text-sm text-destructive">
      {messageFor(error)}
    </p>
  )
}

/** messageFor traduce el error de la API a algo que se pueda leer. */
function messageFor(error: unknown): string {
  if (!(error instanceof ApiError)) {
    return 'No se pudo verificar'
  }
  switch (error.code) {
    case 'invalid_code':
      return 'El código no es correcto. Revisá que el reloj del teléfono esté en hora.'
    case 'unauthorized':
      return 'La verificación se cerró (venció o hubo demasiados intentos). Volvé a poner la contraseña.'
    case 'rate_limited':
      return 'Demasiados intentos seguidos. Esperá un minuto.'
    case 'wrong_stage':
      return 'Este paso ya no corresponde. Volvé a poner la contraseña.'
    case 'network':
      return 'No se pudo hablar con el control plane'
    default:
      return error.message
  }
}

function groupsOf4(secret: string): string {
  return secret.replace(/(.{4})(?=.)/g, '$1 ')
}

function Frame({
  title,
  description,
  children,
}: {
  title: string
  description: string
  children: ReactNode
}) {
  return (
    <div className="grid min-h-dvh place-items-center p-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle className="text-base">{title}</CardTitle>
          <CardDescription>{description}</CardDescription>
        </CardHeader>
        <CardContent>{children}</CardContent>
      </Card>
    </div>
  )
}
