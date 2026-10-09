import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect } from 'react'

import {
  proposals,
  type ApiError,
  type ProposalDetail,
  type ProposalPreview,
  type ProposalRequest,
  type ProposalSummary,
} from '@/lib/api'
import { panelSocket } from '@/lib/panel-socket'
import { isOpen } from '@/lib/proposals'

export const serverProposalsKey = (serverID: string) => ['proposals', 'server', serverID] as const
export const proposalKey = (id: string) => ['proposals', id] as const

/**
 * useLiveProposals vuelve a pedir las propuestas de un servidor cuando el
 * control plane avisa que cambiaron: llegó al agent, la aceptaron, venció.
 */
function useLiveProposals(serverID: string | undefined) {
  const queryClient = useQueryClient()
  useEffect(() => {
    if (!serverID) return
    return panelSocket.subscribeProposals(serverID, () => {
      void queryClient.invalidateQueries({ queryKey: ['proposals'] })
    })
  }, [serverID, queryClient])
}

/** useServerProposals trae las propuestas de un servidor, de la más nueva a la más vieja. */
export function useServerProposals(serverID: string) {
  const query = useQuery<ProposalSummary[], ApiError>({
    queryKey: serverProposalsKey(serverID),
    queryFn: () => proposals.list(serverID),
    refetchInterval: 60_000,
  })
  useLiveProposals(serverID)
  return query
}

/** useProposal trae una propuesta con lo que se va a escribir en el servidor. */
export function useProposal(id: string) {
  const query = useQuery<ProposalDetail, ApiError>({
    queryKey: proposalKey(id),
    queryFn: () => proposals.get(id),
    // Mientras está abierta, por las dudas también se pregunta cada tanto.
    refetchInterval: (q) => (q.state.data && isOpen(q.state.data.status) ? 15_000 : false),
  })
  useLiveProposals(query.data?.server_id)
  return query
}

/** usePreviewProposal arma la vista previa sin guardar nada. */
export function usePreviewProposal(serverID: string) {
  return useMutation<ProposalPreview, ApiError, ProposalRequest>({
    mutationFn: (req) => proposals.preview(serverID, req),
  })
}

/** useCreateProposal guarda la propuesta y se la manda al agent. */
export function useCreateProposal(serverID: string) {
  const queryClient = useQueryClient()
  return useMutation<ProposalDetail, ApiError, ProposalRequest>({
    mutationFn: (req) => proposals.create(serverID, req),
    onSuccess: (created) => {
      queryClient.setQueryData(proposalKey(created.id), created)
      void queryClient.invalidateQueries({ queryKey: serverProposalsKey(serverID) })
    },
  })
}

/** useWithdrawProposal retira una propuesta abierta. */
export function useWithdrawProposal(id: string) {
  const queryClient = useQueryClient()
  return useMutation<ProposalSummary, ApiError, void>({
    mutationFn: () => proposals.withdraw(id),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ['proposals'] }),
  })
}
