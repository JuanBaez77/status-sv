import { cn } from '@/lib/utils'
import type { ServerStatus } from '@/lib/api'

const labels: Record<ServerStatus, string> = {
  online: 'En línea',
  offline: 'Caído',
  never_seen: 'Sin datos',
}

const dots: Record<ServerStatus, string> = {
  online: 'bg-status-online',
  offline: 'bg-status-offline',
  never_seen: 'bg-status-unknown',
}

/**
 * StatusBadge muestra el estado de un servidor. El color va acompañado del
 * texto a propósito: un panel que solo usa color no se entiende si quien mira
 * no distingue rojo de verde.
 */
export function StatusBadge({ status, className }: { status: ServerStatus; className?: string }) {
  return (
    <span className={cn('inline-flex items-center gap-1.5 text-xs', className)}>
      <span className={cn('size-2 rounded-full', dots[status])} aria-hidden />
      {labels[status]}
    </span>
  )
}
