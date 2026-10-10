import { LogOut, Server } from 'lucide-react'
import type { ReactNode } from 'react'
import { Link, useLocation } from 'wouter'

import { Button } from '@/components/ui/button'
import { useLogout } from '@/hooks/use-session'
import type { Session } from '@/lib/api'
import { cn } from '@/lib/utils'

type AppShellProps = {
  session: Session
  children: ReactNode
}

/** lowRecoveryCodes es desde cuántos códigos de recuperación se avisa. */
const lowRecoveryCodes = 3

/**
 * RecoveryCodesWarning avisa cuando quedan pocos códigos de recuperación: con
 * cero, perder el teléfono obliga a resetear el segundo factor por SSH.
 */
function RecoveryCodesWarning({ left }: { left: number | undefined }) {
  if (left === undefined || left > lowRecoveryCodes) return null
  return (
    <p
      role="status"
      className="mb-4 rounded-md border border-border px-3 py-2 text-sm text-muted-foreground"
    >
      {left === 0
        ? 'No te quedan códigos de recuperación.'
        : `Te ${left === 1 ? 'queda 1 código' : `quedan ${left} códigos`} de recuperación.`}{' '}
      Para generar nuevos hay que resetear el segundo factor con <code>server user-totp-reset</code>.
    </p>
  )
}

/** NavLink es una sección del panel; la actual queda marcada. */
function NavLink({ href, children }: { href: string; children: ReactNode }) {
  const [location] = useLocation()
  const active =
    href === '/' ? location === '/' || location.startsWith('/servers') : location.startsWith(href)
  return (
    <Link
      href={href}
      aria-current={active ? 'page' : undefined}
      className={cn(
        'rounded-md px-2 py-1 text-muted-foreground hover:text-foreground',
        active && 'bg-muted text-foreground',
      )}
    >
      {children}
    </Link>
  )
}

/** AppShell es el marco del panel: la barra de arriba y el contenido. */
export function AppShell({ session, children }: AppShellProps) {
  const logout = useLogout()

  return (
    <div className="min-h-dvh">
      <header className="border-b border-border">
        {/* En una pantalla angosta las secciones bajan a un segundo renglón:
            con cuatro ya no entran al lado del nombre y del botón de salir. */}
        <div className="mx-auto flex min-h-14 max-w-6xl flex-wrap items-center gap-x-4 gap-y-1 px-4 py-2 sm:py-0">
          <Link href="/" className="flex items-center gap-2 font-medium whitespace-nowrap">
            <Server className="size-4 text-primary" aria-hidden />
            status-sv
          </Link>

          <nav
            aria-label="Secciones"
            className="order-last flex w-full items-center gap-1 text-sm sm:order-none sm:w-auto"
          >
            <NavLink href="/">Servidores</NavLink>
            <NavLink href="/projects">Proyectos</NavLink>
            <NavLink href="/audit">Auditoría</NavLink>
            <NavLink href="/tokens">Tokens</NavLink>
          </nav>

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

      <main className="mx-auto max-w-6xl px-4 py-6">
        <RecoveryCodesWarning left={session.recovery_codes_left} />
        {children}
      </main>
    </div>
  )
}
