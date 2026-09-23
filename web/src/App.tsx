import { Route, Switch } from 'wouter'

import { AppShell } from '@/components/app-shell'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useSession } from '@/hooks/use-session'

/**
 * App decide qué se ve: mientras no haya sesión, el panel no muestra nada.
 *
 * El formulario de login y la lista de servidores llegan en el PR siguiente;
 * esta versión deja armado el marco, el router y el cliente de la API.
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
    return (
      <Centered>
        <Card className="w-full max-w-sm">
          <CardHeader>
            <CardTitle>Iniciá sesión</CardTitle>
            <CardDescription>El formulario llega en el próximo cambio.</CardDescription>
          </CardHeader>
        </Card>
      </Centered>
    )
  }

  return (
    <AppShell session={session}>
      <Switch>
        <Route path="/">
          <Card>
            <CardHeader>
              <CardTitle>Servidores</CardTitle>
              <CardDescription>La lista llega en el próximo cambio.</CardDescription>
            </CardHeader>
          </Card>
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
