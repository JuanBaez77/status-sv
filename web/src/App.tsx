import { Route, Switch } from 'wouter'

import { AppShell } from '@/components/app-shell'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useSession } from '@/hooks/use-session'
import { LoginPage } from '@/pages/login'
import { ServersPage } from '@/pages/servers'

/** App decide qué se ve: mientras no haya sesión, solo el login. */
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

  return (
    <AppShell session={session}>
      <Switch>
        <Route path="/" component={ServersPage} />

        <Route path="/servers/:id">
          {(params) => (
            <Card>
              <CardHeader>
                <CardTitle>Servidor</CardTitle>
                <CardDescription>
                  Los gráficos de este servidor llegan en el próximo cambio.
                </CardDescription>
              </CardHeader>
              <CardContent>
                <p className="font-mono text-xs text-muted-foreground">{params.id}</p>
              </CardContent>
            </Card>
          )}
        </Route>

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
