/** Formatos que usa el panel. Todo en castellano y con la coma decimal. */

const numberFormat = new Intl.NumberFormat('es-AR', { maximumFractionDigits: 1 })

/** percent formatea un porcentaje ya calculado: 42,7 %. */
export function percent(value: number): string {
  return `${numberFormat.format(value)} %`
}

/** ratio formatea una parte sobre un total como porcentaje. */
export function ratio(part: number, total: number): string {
  if (total <= 0) return '—'
  return percent((part / total) * 100)
}

const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'] as const

/** bytes formatea un tamaño en la unidad binaria que le queda cómoda. */
export function bytes(value: number): string {
  if (!Number.isFinite(value) || value < 0) return '—'

  let size = value
  let unit = 0
  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024
    unit++
  }
  return `${numberFormat.format(size)} ${units[unit]}`
}

/** perSecond formatea una tasa de red. */
export function perSecond(value: number): string {
  return `${bytes(value)}/s`
}

/**
 * since dice hace cuánto pasó algo, en palabras: "hace 12 s", "hace 3 min".
 * Para un panel que se mira de reojo, es más útil que una fecha.
 */
export function since(iso: string | null, now: Date = new Date()): string {
  if (iso === null) return 'nunca'

  const then = new Date(iso)
  const seconds = Math.round((now.getTime() - then.getTime()) / 1000)
  if (Number.isNaN(seconds)) return '—'
  if (seconds < 0) return 'recién'

  if (seconds < 60) return `hace ${seconds} s`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `hace ${minutes} min`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `hace ${hours} h`
  const days = Math.floor(hours / 24)
  return `hace ${days} d`
}
