import { useQuery, type UseQueryResult } from '@tanstack/react-query'
import { RefreshCw, ScrollText } from 'lucide-react'
import { useEffect, useRef } from 'react'

import { servers, type ApiError, type LogKind, type ServerLogs } from '@/lib/api'
import { cn } from '@/lib/utils'

/** logLines es cuántos renglones se piden: suficientes para ver por qué se cayó algo. */
const logLines = 200

const timeFormat = new Intl.DateTimeFormat('es-AR', {
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false,
})

/** reasons explica en castellano por qué el agent no trajo los logs. */
const reasons: Record<string, string> = {
  unknown_target: 'El agent ya no ve esto en el servidor.',
  busy: 'El agent está atendiendo otros pedidos; probá en unos segundos.',
  timeout: 'Leer los logs tardó demasiado.',
}

/** LogsButton abre y cierra el panel de logs de una unidad. */
export function LogsButton({
  open,
  onToggle,
  name,
  controls,
}: {
  open: boolean
  onToggle: () => void
  name: string
  controls: string
}) {
  return (
    <button
      type="button"
      onClick={onToggle}
      aria-expanded={open}
      aria-controls={controls}
      aria-label={`${open ? 'Cerrar' : 'Ver'} los logs de ${name}`}
      className="ml-auto inline-flex shrink-0 items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
    >
      <ScrollText className="size-3" aria-hidden />
      Logs
    </button>
  )
}

/**
 * LogsPanel muestra los últimos renglones de log de una unidad. Se piden recién
 * al abrirlo y se vuelven a pedir solo con Actualizar: un log no se sigue en
 * vivo.
 */
export function LogsPanel({
  id,
  serverID,
  kind,
  target,
}: {
  id: string
  serverID: string
  kind: LogKind
  target: string
}) {
  const query = useQuery<ServerLogs, ApiError>({
    queryKey: ['server-logs', serverID, kind, target],
    queryFn: () => servers.logs(serverID, kind, target, logLines),
    retry: false,
    staleTime: Infinity,
    gcTime: 0,
  })

  const box = useRef<HTMLDivElement>(null)
  // Lo más nuevo va abajo, como en una terminal: el cuadro arranca ahí.
  useEffect(() => {
    if (box.current !== null) box.current.scrollTop = box.current.scrollHeight
  }, [query.data])

  return (
    <div id={id} className="mt-1 flex w-full flex-col gap-1">
      <div className="flex items-center justify-between gap-2 text-xs text-muted-foreground">
        <span>
          {query.data?.status === 'ok'
            ? `Últimos ${query.data.lines.length} renglones${query.data.truncated ? ' (los más viejos no entraron)' : ''}`
            : 'Últimos renglones del log'}
        </span>
        <button
          type="button"
          onClick={() => void query.refetch()}
          disabled={query.isFetching}
          className="inline-flex items-center gap-1 hover:text-foreground disabled:opacity-50"
        >
          <RefreshCw className={cn('size-3', query.isFetching && 'animate-spin')} aria-hidden />
          Actualizar
        </button>
      </div>

      <div
        ref={box}
        className="max-h-72 overflow-auto rounded-md bg-muted p-2 font-mono text-[11px] leading-relaxed"
        aria-live="polite"
        aria-busy={query.isFetching}
      >
        <LogsBody query={query} />
      </div>
    </div>
  )
}

function LogsBody({ query }: { query: UseQueryResult<ServerLogs, ApiError> }) {
  if (query.isPending) {
    return <p className="text-muted-foreground">Pidiéndole los logs al agent…</p>
  }
  if (query.error !== null) {
    return <p className="text-destructive">{query.error.message}</p>
  }

  const logs = query.data
  if (logs.status !== 'ok') {
    return (
      <p className="text-destructive">
        {reasons[logs.code ?? ''] ?? logs.message ?? 'El agent no pudo traer los logs.'}
      </p>
    )
  }
  if (logs.lines.length === 0) {
    return <p className="text-muted-foreground">No hay renglones.</p>
  }

  return (
    <ol>
      {logs.lines.map((line, index) => (
        <li
          key={index}
          className={cn('whitespace-pre-wrap break-words', line.stream === 'stderr' && 'text-destructive')}
        >
          {line.ts !== undefined && (
            <time dateTime={line.ts} className="mr-2 text-muted-foreground">
              {timeFormat.format(new Date(line.ts))}
            </time>
          )}
          {line.text}
        </li>
      ))}
    </ol>
  )
}
