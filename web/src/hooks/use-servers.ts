import { useQuery } from '@tanstack/react-query'

import { servers, type ApiError, type ServerSummary } from '@/lib/api'

export const serversKey = ['servers'] as const

/**
 * useServers trae la lista de servidores.
 *
 * Se refresca sola cada 10 segundos, que es cada cuánto manda el agent. El
 * canal en vivo empuja muestras de los servidores suscriptos, pero todavía no
 * avisa cuando uno pasa a caído: eso solo se ve preguntando de nuevo.
 */
export function useServers() {
  return useQuery<ServerSummary[], ApiError>({
    queryKey: serversKey,
    queryFn: servers.list,
    refetchInterval: 10_000,
  })
}
