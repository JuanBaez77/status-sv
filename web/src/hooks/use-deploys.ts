import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect } from 'react'

import {
  deploys,
  type ApiError,
  type DeployDetail,
  type DeployKind,
  type DeployRequest,
  type DeploySummary,
} from '@/lib/api'
import { active } from '@/lib/deploys'
import { panelSocket } from '@/lib/panel-socket'

export const deployHistoryKey = (projectID: string) => ['deploys', 'project', projectID] as const
export const deployKey = (id: string) => ['deploys', id] as const

/**
 * useDeployHistory trae los deploys de un proyecto y los mantiene al día con
 * el canal en vivo: cada cambio de un deploy del servidor vuelve a pedir la
 * historia y el deploy que cambió.
 */
export function useDeployHistory(projectID: string, serverID: string | undefined) {
  const queryClient = useQueryClient()
  const query = useQuery<{ deploys: DeploySummary[] }, ApiError>({
    queryKey: deployHistoryKey(projectID),
    queryFn: () => deploys.history(projectID),
    // Por las dudas, mientras hay uno activo también se pregunta cada tanto.
    refetchInterval: (q) => (q.state.data?.deploys.some((d) => active(d.status)) ? 10_000 : 60_000),
  })

  useEffect(() => {
    if (!serverID) return
    return panelSocket.subscribeDeploys(serverID, (event) => {
      if (event.project_id !== null && event.project_id !== projectID) return
      void queryClient.invalidateQueries({ queryKey: deployHistoryKey(projectID) })
      void queryClient.invalidateQueries({ queryKey: deployKey(event.deploy_id) })
    })
  }, [projectID, serverID, queryClient])

  return query
}

/** useDeploy trae un deploy con sus pasos. */
export function useDeploy(id: string | null) {
  return useQuery<DeployDetail, ApiError>({
    queryKey: deployKey(id ?? ''),
    queryFn: () => deploys.get(id as string),
    enabled: id !== null,
    refetchInterval: (q) => (q.state.data && active(q.state.data.status) ? 10_000 : false),
  })
}

/** useStartDeploy pide un deploy, un rollback o un primer deploy. */
export function useStartDeploy(projectID: string) {
  const queryClient = useQueryClient()
  return useMutation<DeployDetail, ApiError, { kind: DeployKind; request: DeployRequest }>({
    mutationFn: ({ kind, request }) => {
      switch (kind) {
        case 'rollback':
          return deploys.rollback(projectID, request)
        case 'bootstrap':
          return deploys.bootstrap(projectID, request)
        default:
          return deploys.start(projectID, request)
      }
    },
    onSuccess: (deploy) => {
      queryClient.setQueryData(deployKey(deploy.id), deploy)
      void queryClient.invalidateQueries({ queryKey: deployHistoryKey(projectID) })
    },
  })
}
