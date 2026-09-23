/**
 * Los rangos de tiempo que ofrece la vista de un servidor.
 *
 * La API elige sola la resolución: hasta 3 horas contesta con las muestras
 * crudas y más allá con el agregado por minuto, que es lo único que queda
 * pasadas las 48 horas. Por eso "1 hora" es el único rango que llega en crudo,
 * y el único al que se le pueden pegar atrás las muestras en vivo.
 */

export type RangeKey = '1h' | '6h' | '24h' | '7d'

export type Range = {
  key: RangeKey
  label: string
  seconds: number
}

const hour = 60 * 60

export const ranges: readonly Range[] = [
  { key: '1h', label: '1 hora', seconds: hour },
  { key: '6h', label: '6 horas', seconds: 6 * hour },
  { key: '24h', label: '24 horas', seconds: 24 * hour },
  { key: '7d', label: '7 días', seconds: 7 * 24 * hour },
]

export const defaultRange: RangeKey = '1h'

/** rawLimitSeconds es hasta dónde contesta la API con muestras crudas. */
export const rawLimitSeconds = 3 * hour

export function rangeSeconds(key: RangeKey): number {
  return ranges.find((range) => range.key === key)?.seconds ?? hour
}

/** isRangeKey filtra lo que venga de la barra de direcciones. */
export function isRangeKey(value: string | null): value is RangeKey {
  return value !== null && ranges.some((range) => range.key === value)
}
