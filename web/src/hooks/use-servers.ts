import { useQuery } from '@tanstack/react-query'

import { servers, type ApiError, type ServerSummary } from '@/lib/api'

export const serversKey = ['servers'] as const

/**
 * useServers trae la lista de servidores.
 *
 * Mientras el panel no escuche el WebSocket (llega con la vista de un
 * servidor), la lista se refresca sola cada 10 segundos, que es el intervalo
 * con el que el agent manda sus muestras.
 */
export function useServers() {
  return useQuery<ServerSummary[], ApiError>({
    queryKey: serversKey,
    queryFn: servers.list,
    refetchInterval: 10_000,
  })
}
