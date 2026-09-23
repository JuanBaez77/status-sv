import { cn } from '@/lib/utils'
import type { PanelStatus } from '@/lib/panel-socket'

const labels: Record<PanelStatus, string> = {
  connecting: 'Conectando…',
  live: 'En vivo',
  reconnecting: 'Reconectando…',
}

const dots: Record<PanelStatus, string> = {
  connecting: 'bg-status-unknown',
  live: 'bg-status-online',
  reconnecting: 'bg-status-offline',
}

/**
 * LiveIndicator dice si el canal en vivo está al día. Sin esto, un panel
 * desconectado se ve igual que un servidor tranquilo: los números quietos.
 */
export function LiveIndicator({ status, className }: { status: PanelStatus; className?: string }) {
  return (
    <span className={cn('inline-flex items-center gap-1.5 text-xs text-muted-foreground', className)}>
      <span className={cn('size-2 rounded-full', dots[status])} aria-hidden />
      {labels[status]}
    </span>
  )
}
