import { Route, Switch } from 'wouter'

import { AppShell } from '@/components/app-shell'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useSession } from '@/hooks/use-session'
import { LoginPage } from '@/pages/login'
import { ServerPage } from '@/pages/server'
import { ServersPage } from '@/pages/servers'
import { TotpEnrollPage, TotpVerifyPage } from '@/pages/totp'

/**
 * App decide qué se ve: mientras no haya sesión, solo el login; con la sesión
 * a medias, el segundo factor; con la completa, el panel.
 */
export default function App() {
  const { session, loading, unreachable } = useSession()

  if (loading) {
    return <Centered>Cargando…</Centered>
  }

  if (unreachable) {
    return (
      <Centered>
        <Card className="w-full max-w-sm">
          <CardHeader>
            <CardTitle>No se puede hablar con el control plane</CardTitle>
            <CardDescription>Revisá que esté levantado y volvé a intentar.</CardDescription>
          </CardHeader>
        </Card>
      </Centered>
    )
  }

  if (!session) {
    return <LoginPage />
  }

  // Con la contraseña sola la sesión queda a medias: falta el segundo factor.
  if (session.stage === 'totp_pending') {
    return <TotpVerifyPage session={session} />
  }
  if (session.stage !== 'full') {
    return <TotpEnrollPage session={session} />
  }

  return (
    <AppShell session={session}>
      <Switch>
        <Route path="/" component={ServersPage} />

        <Route path="/servers/:id">{(params) => <ServerPage id={params.id} />}</Route>

        <Route>
          <Card>
            <CardHeader>
              <CardTitle>No existe esa página</CardTitle>
            </CardHeader>
            <CardContent>
              <a className="text-sm text-primary underline" href="/">
                Volver al panel
              </a>
            </CardContent>
          </Card>
        </Route>
      </Switch>
    </AppShell>
  )
}

function Centered({ children }: { children: React.ReactNode }) {
  return (
    <div className="grid min-h-dvh place-items-center p-4 text-sm text-muted-foreground">
      {children}
    </div>
  )
}
