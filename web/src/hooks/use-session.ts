import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ApiError, auth, type Session } from '@/lib/api'

export const sessionKey = ['session'] as const

/**
 * useSession dice si hay sesión abierta. Un 401 no es un error que haya que
 * mostrar: es la respuesta esperada cuando todavía no se inició sesión.
 */
export function useSession() {
  const query = useQuery<Session, ApiError>({
    queryKey: sessionKey,
    queryFn: auth.me,
    retry: false,
  })

  return {
    session: query.data ?? null,
    loading: query.isPending,
    // Un error de red no es lo mismo que no tener sesión: con el primero hay
    // que avisar, con el segundo, mostrar el login.
    unreachable: query.error?.status === 0,
    error: query.error ?? null,
  }
}

/** useLogout cierra la sesión y limpia lo que haya en memoria. */
export function useLogout() {
  const client = useQueryClient()
  return useMutation({
    mutationFn: auth.logout,
    onSettled: () => {
      client.clear()
    },
  })
}
