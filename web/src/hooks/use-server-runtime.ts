import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect } from 'react'

import { servers, type ApiError, type ServerRuntime } from '@/lib/api'
import { panelSocket } from '@/lib/panel-socket'

export const runtimeKey = (serverID: string) => ['server-runtime', serverID] as const

/**
 * useServerRuntime trae lo que corre en un servidor y lo mantiene al día con
 * el canal en vivo: cada foto que guarda el control plane reemplaza a la
 * anterior en el caché, sin volver a pedir nada.
 *
 * Se vuelve a pedir cada cinco minutos igual, por si el canal se cortó un rato
 * y se perdió alguna.
 */
export function useServerRuntime(serverID: string) {
  const queryClient = useQueryClient()

  useEffect(
    () =>
      panelSocket.subscribeRuntime(serverID, (message) => {
        queryClient.setQueryData(runtimeKey(serverID), message.runtime)
      }),
    [serverID, queryClient],
  )

  return useQuery<ServerRuntime, ApiError>({
    queryKey: runtimeKey(serverID),
    queryFn: () => servers.runtime(serverID),
    refetchInterval: 5 * 60_000,
  })
}
