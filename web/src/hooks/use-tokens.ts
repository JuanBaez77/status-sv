import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import {
  mcpTokens,
  type ApiError,
  type CreatedToken,
  type CreateTokenRequest,
  type McpToken,
  type McpTokenList,
} from '@/lib/api'

export const tokensKey = ['tokens'] as const

/** useTokens trae los tokens del MCP, del más nuevo al más viejo. */
export function useTokens() {
  return useQuery<McpTokenList, ApiError>({
    queryKey: tokensKey,
    queryFn: mcpTokens.list,
    // El último uso cambia solo, cuando Claude Code usa el token.
    refetchInterval: 60_000,
  })
}

/** useCreateToken crea un token. El valor llega solo en esta respuesta. */
export function useCreateToken() {
  const queryClient = useQueryClient()
  return useMutation<CreatedToken, ApiError, CreateTokenRequest>({
    mutationFn: mcpTokens.create,
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: tokensKey }),
  })
}

/** useRevokeToken revoca un token: deja de servir en el próximo pedido. */
export function useRevokeToken() {
  const queryClient = useQueryClient()
  return useMutation<McpToken, ApiError, string>({
    mutationFn: mcpTokens.revoke,
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: tokensKey }),
  })
}
