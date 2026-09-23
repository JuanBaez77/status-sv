import { cn } from '@/lib/utils'
import { percent } from '@/lib/format'

/**
 * UsageBar es una barra de uso con su número al lado. Pasado el 90 % se pinta
 * como problema: un disco lleno es lo que rompe un servidor de noche.
 */
export function UsageBar({
  label,
  value,
  detail,
  className,
}: {
  label: string
  value: number
  detail?: string
  className?: string
}) {
  const clamped = Math.min(Math.max(value, 0), 100)
  const alarming = clamped >= 90

  return (
    <div className={cn('flex flex-col gap-1', className)}>
      <div className="flex items-baseline justify-between gap-2 text-xs">
        <span className="text-muted-foreground">{label}</span>
        <span className={cn('tabular-nums', alarming && 'text-destructive')}>{percent(clamped)}</span>
      </div>

      <div
        className="h-1.5 w-full overflow-hidden rounded-full bg-secondary"
        role="meter"
        aria-label={label}
        aria-valuenow={Math.round(clamped)}
        aria-valuemin={0}
        aria-valuemax={100}
      >
        <div
          className={cn('h-full rounded-full transition-[width]', alarming ? 'bg-destructive' : 'bg-primary')}
          style={{ width: `${clamped}%` }}
        />
      </div>

      {detail !== undefined && <p className="text-xs text-muted-foreground">{detail}</p>}
    </div>
  )
}
