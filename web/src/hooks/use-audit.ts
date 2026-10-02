import { useInfiniteQuery } from '@tanstack/react-query'

import { audit, type ApiError, type AuditPage } from '@/lib/api'
import { auditQuery, type AuditFilters } from '@/lib/audit'

/**
 * useAudit trae la auditoría con los filtros dados, de a una página: cada
 * página pide la siguiente con el cursor que devolvió la anterior (ADR-020).
 * No se refresca sola: una lista que se mueve mientras se lee molesta más de
 * lo que ayuda, y para eso está el botón.
 */
export function useAudit(filters: AuditFilters) {
  return useInfiniteQuery<AuditPage, ApiError, AuditPage[], readonly unknown[], string | null>({
    queryKey: ['audit', filters],
    queryFn: ({ pageParam }) => audit.list(auditQuery(filters, pageParam)),
    initialPageParam: null,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
    select: (data) => data.pages,
  })
}
