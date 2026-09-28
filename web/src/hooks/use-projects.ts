import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect } from 'react'

import { projects, type ApiError, type ProjectDetail, type ProjectList } from '@/lib/api'
import { panelSocket } from '@/lib/panel-socket'

export const projectsKey = ['projects'] as const
export const projectKey = (id: string) => ['projects', id] as const

/**
 * useLiveProjects mantiene al día los proyectos de unos servidores: cuando
 * cambian sus proyectos declarados o lo que corre en ellos, el estado de los
 * proyectos puede cambiar, así que se vuelven a pedir. El canal solo avisa;
 * el estado lo calcula el control plane.
 */
function useLiveProjects(serverIDs: string[]) {
  const queryClient = useQueryClient()
  const key = serverIDs.join(',')

  useEffect(() => {
    const refresh = () => void queryClient.invalidateQueries({ queryKey: projectsKey })
    const stops = key
      .split(',')
      .filter((id) => id !== '')
      .flatMap((id) => [
        panelSocket.subscribeProjects(id, refresh),
        panelSocket.subscribeRuntime(id, refresh),
      ])
    return () => stops.forEach((stop) => stop())
  }, [key, queryClient])
}

/**
 * useProjects trae todos los proyectos declarados. Se refresca con el canal en
 * vivo de los servidores que tienen proyectos, y cada minuto por las dudas.
 */
export function useProjects() {
  const query = useQuery<ProjectList, ApiError>({
    queryKey: projectsKey,
    queryFn: projects.list,
    refetchInterval: 60_000,
  })
  const servers = [...new Set(query.data?.projects.map((p) => p.server_id) ?? [])].sort()
  useLiveProjects(servers)
  return query
}

/** useProject trae un proyecto con lo que corre de él. */
export function useProject(id: string) {
  const query = useQuery<ProjectDetail, ApiError>({
    queryKey: projectKey(id),
    queryFn: () => projects.get(id),
    refetchInterval: 60_000,
  })
  useLiveProjects(query.data ? [query.data.server_id] : [])
  return query
}
