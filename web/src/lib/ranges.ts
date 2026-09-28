/**
 * Los rangos de tiempo que ofrece la vista de un servidor.
 *
 * La API elige sola la resolución: hasta 3 horas contesta con las muestras
 * crudas, hasta 48 horas con el agregado por minuto y más allá con el agregado
 * por hora, que es lo único que queda pasados los 30 días. Por eso "1 hora" es
 * el único rango que llega en crudo, y el único al que se le pueden pegar atrás
 * las muestras en vivo.
 */

export type RangeKey = '1h' | '6h' | '24h' | '7d' | '30d' | '90d' | '1y'

export type Range = {
  key: RangeKey
  label: string
  seconds: number
}

const hour = 60 * 60
const day = 24 * hour

export const ranges: readonly Range[] = [
  { key: '1h', label: '1 hora', seconds: hour },
  { key: '6h', label: '6 horas', seconds: 6 * hour },
  { key: '24h', label: '24 horas', seconds: day },
  { key: '7d', label: '7 días', seconds: 7 * day },
  { key: '30d', label: '30 días', seconds: 30 * day },
  { key: '90d', label: '90 días', seconds: 90 * day },
  { key: '1y', label: '1 año', seconds: 365 * day },
]

export const defaultRange: RangeKey = '1h'

/** rawLimitSeconds es hasta dónde contesta la API con muestras crudas. */
export const rawLimitSeconds = 3 * hour

/** minuteLimitSeconds es hasta dónde contesta con el agregado por minuto. */
export const minuteLimitSeconds = 2 * day

export function rangeSeconds(key: RangeKey): number {
  return ranges.find((range) => range.key === key)?.seconds ?? hour
}

/** isRangeKey filtra lo que venga de la barra de direcciones. */
export function isRangeKey(value: string | null): value is RangeKey {
  return value !== null && ranges.some((range) => range.key === value)
}
