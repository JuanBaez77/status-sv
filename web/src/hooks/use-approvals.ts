import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { approvals, type ApiError, type Approval, type ApprovalStatus, type ApproveRequest } from '@/lib/api'

export const approvalsKey = ['approvals'] as const

/**
 * useApprovals trae los pedidos de escritura del MCP (ADR-026), del más nuevo
 * al más viejo. Los pide un LLM en cualquier momento: se miran seguido.
 */
export function useApprovals(status?: ApprovalStatus) {
  return useQuery<Approval[], ApiError>({
    queryKey: [...approvalsKey, status ?? 'all'],
    queryFn: () => approvals.list(status),
    refetchInterval: 10_000,
  })
}

/** useApprove aprueba un pedido con el nombre escrito y un código TOTP nuevo. */
export function useApprove() {
  const queryClient = useQueryClient()
  return useMutation<Approval, ApiError, ApproveRequest & { id: string }>({
    mutationFn: approvals.approve,
    // Haya salido o no, el pedido pudo cambiar: aprobado, cancelado.
    onSettled: () => void queryClient.invalidateQueries({ queryKey: approvalsKey }),
  })
}

/** useReject rechaza un pedido. No pide código: frenar tiene que ser fácil. */
export function useReject() {
  const queryClient = useQueryClient()
  return useMutation<Approval, ApiError, { id: string; note: string }>({
    mutationFn: approvals.reject,
    onSettled: () => void queryClient.invalidateQueries({ queryKey: approvalsKey }),
  })
}
