import { LogOut, Server } from 'lucide-react'
import type { ReactNode } from 'react'
import { Link } from 'wouter'

import { Button } from '@/components/ui/button'
import { useLogout } from '@/hooks/use-session'
import type { Session } from '@/lib/api'

type AppShellProps = {
  session: Session
  children: ReactNode
}

/** AppShell es el marco del panel: la barra de arriba y el contenido. */
export function AppShell({ session, children }: AppShellProps) {
  const logout = useLogout()

  return (
    <div className="min-h-dvh">
      <header className="border-b border-border">
        <div className="mx-auto flex h-14 max-w-6xl items-center gap-4 px-4">
          <Link href="/" className="flex items-center gap-2 font-medium">
            <Server className="size-4 text-primary" aria-hidden />
            status-sv
          </Link>

          <div className="ml-auto flex items-center gap-3 text-sm text-muted-foreground">
            <span>{session.username}</span>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => logout.mutate()}
              disabled={logout.isPending}
            >
              <LogOut className="size-4" aria-hidden />
              Salir
            </Button>
          </div>
        </div>
      </header>

      <main className="mx-auto max-w-6xl px-4 py-6">{children}</main>
    </div>
  )
}
