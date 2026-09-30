import type { ProjectStatus } from '@/lib/api'
import { cn } from '@/lib/utils'

const labels: Record<ProjectStatus, string> = {
  ok: 'Funcionando',
  degraded: 'Con problemas',
  down: 'Caído',
  unknown: 'Sin datos',
}

const dots: Record<ProjectStatus, string> = {
  ok: 'bg-status-online',
  degraded: 'bg-status-warning',
  down: 'bg-status-offline',
  unknown: 'bg-status-unknown',
}

/** ProjectStatusBadge muestra el estado de un proyecto, con color y texto. */
export function ProjectStatusBadge({
  status,
  className,
}: {
  status: ProjectStatus
  className?: string
}) {
  return (
    <span className={cn('inline-flex items-center gap-1.5 text-xs', className)}>
      <span className={cn('size-2 rounded-full', dots[status])} aria-hidden />
      {labels[status]}
    </span>
  )
}
