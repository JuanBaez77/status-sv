import { keepPreviousData, useQuery } from '@tanstack/react-query'

import { servers, type ApiError, type ServerMetrics } from '@/lib/api'
import { minuteLimitSeconds, rangeSeconds, rawLimitSeconds, type RangeKey } from '@/lib/ranges'

export const metricsKey = (serverID: string, range: RangeKey) =>
  ['server-metrics', serverID, range] as const

/**
 * useServerMetrics trae las series de un servidor para un rango.
 *
 * El rango en crudo se mantiene al día con el canal en vivo, así que se vuelve
 * a pedir cada varios minutos nada más que para correr la ventana. El agregado
 * por minuto no llega por el canal: ese se refresca cada minuto, que es
 * exactamente lo que tarda en aparecer un punto nuevo. El agregado por hora
 * cambia poco y puede traer miles de puntos, así que va cada cinco minutos.
 *
 * Al cambiar de rango se siguen mostrando los datos anteriores hasta que llegan
 * los nuevos: un gráfico que se vacía y se vuelve a llenar marea.
 */
export function useServerMetrics(serverID: string, range: RangeKey) {
  const seconds = rangeSeconds(range)

  return useQuery<ServerMetrics, ApiError>({
    queryKey: metricsKey(serverID, range),
    queryFn: () => {
      const to = new Date()
      return servers.metrics(serverID, new Date(to.getTime() - seconds * 1000), to)
    },
    refetchInterval: seconds <= rawLimitSeconds || seconds > minuteLimitSeconds ? 5 * 60_000 : 60_000,
    placeholderData: keepPreviousData,
  })
}
