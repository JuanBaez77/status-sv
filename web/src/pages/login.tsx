import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'

import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { sessionKey } from '@/hooks/use-session'
import { ApiError, auth } from '@/lib/api'

/**
 * LoginPage es lo único que se ve sin sesión.
 *
 * La contraseña no se guarda en ningún lado: se manda y se olvida. Lo que queda
 * es la cookie que pone el control plane, que este código no puede leer.
 */
export function LoginPage() {
  const client = useQueryClient()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')

  const login = useMutation({
    mutationFn: () => auth.login(username, password),
    onSuccess: (session) => {
      setPassword('')
      client.setQueryData(sessionKey, session)
    },
  })

  function onSubmit(event: FormEvent) {
    event.preventDefault()
    login.mutate()
  }

  return (
    <div className="grid min-h-dvh place-items-center p-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle className="text-base">status-sv</CardTitle>
          <CardDescription>Entrá para ver tus servidores.</CardDescription>
        </CardHeader>

        <CardContent>
          <form onSubmit={onSubmit} className="flex flex-col gap-4">
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="username">Usuario</Label>
              <Input
                id="username"
                name="username"
                autoComplete="username"
                autoFocus
                required
                value={username}
                onChange={(event) => setUsername(event.target.value)}
              />
            </div>

            <div className="flex flex-col gap-1.5">
              <Label htmlFor="password">Contraseña</Label>
              <Input
                id="password"
                name="password"
                type="password"
                autoComplete="current-password"
                required
                value={password}
                onChange={(event) => setPassword(event.target.value)}
              />
            </div>

            {login.isError && (
              <p role="alert" className="text-sm text-destructive">
                {messageFor(login.error)}
              </p>
            )}

            <Button type="submit" disabled={login.isPending}>
              {login.isPending ? 'Entrando…' : 'Entrar'}
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}

/** messageFor traduce el error de la API a algo que se pueda leer. */
function messageFor(error: unknown): string {
  if (!(error instanceof ApiError)) {
    return 'No se pudo iniciar sesión'
  }
  switch (error.code) {
    case 'invalid_credentials':
      return 'Usuario o contraseña incorrectos'
    case 'rate_limited':
      return 'Demasiados intentos seguidos. Esperá un minuto.'
    case 'network':
      return 'No se pudo hablar con el control plane'
    default:
      return error.message
  }
}
