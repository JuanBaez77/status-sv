import { Link } from 'wouter'

import { StatusBadge } from '@/components/status-badge'
import { UsageBar } from '@/components/usage-bar'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useServers } from '@/hooks/use-servers'
import type { ServerSummary } from '@/lib/api'
import { bytes, since } from '@/lib/format'

/** ServersPage es la vista principal: todos los servidores de un vistazo. */
export function ServersPage() {
  const { data, isPending, error } = useServers()

  if (isPending) {
    return <p className="text-sm text-muted-foreground">Cargando servidores…</p>
  }

  if (error) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>No se pudieron traer los servidores</CardTitle>
          <CardDescription>{error.message}</CardDescription>
        </CardHeader>
      </Card>
    )
  }

  if (data.length === 0) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Todavía no hay servidores</CardTitle>
          <CardDescription>
            Generá un token con <code className="font-mono">server enroll-token</code> e instalá el
            agent en el VPS.
          </CardDescription>
        </CardHeader>
      </Card>
    )
  }

  return (
    <section className="flex flex-col gap-4">
      <h1 className="text-lg font-medium">Servidores</h1>

      <ul className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {data.map((server) => (
          <li key={server.id}>
            <ServerCard server={server} />
          </li>
        ))}
      </ul>
    </section>
  )
}

function ServerCard({ server }: { server: ServerSummary }) {
  const sample = server.sample

  return (
    <Card className="h-full transition-colors hover:border-primary">
      <CardHeader>
        <div className="flex items-start justify-between gap-2">
          <div className="min-w-0">
            <CardTitle className="truncate">
              <Link href={`/servers/${server.id}`} className="hover:underline">
                {server.name}
              </Link>
            </CardTitle>
            <CardDescription className="truncate">{server.hostname}</CardDescription>
          </div>
          <StatusBadge status={server.status} />
        </div>
      </CardHeader>

      <CardContent className="flex flex-col gap-3">
        {sample === null ? (
          <p className="text-xs text-muted-foreground">
            Sin métricas todavía. Último contacto: {since(server.last_seen_at)}.
          </p>
        ) : (
          <>
            <UsageBar
              label="CPU"
              value={sample.cpu_used_percent}
              detail={`${sample.cpu_cores} núcleos · carga ${sample.load1.toFixed(2)}`}
            />
            <UsageBar
              label="Memoria"
              value={(sample.mem_used_bytes / sample.mem_total_bytes) * 100}
              detail={`${bytes(sample.mem_used_bytes)} de ${bytes(sample.mem_total_bytes)}`}
            />
            {sample.worst_disk !== null && (
              <UsageBar
                label={`Disco ${sample.worst_disk.mountpoint}`}
                value={sample.worst_disk.used_percent}
                detail={`quedan ${bytes(sample.worst_disk.available_bytes)}`}
              />
            )}
          </>
        )}

        <p className="text-xs text-muted-foreground">
          Último latido: {since(server.last_seen_at)} · agent {server.agent_version || '—'}
        </p>
      </CardContent>
    </Card>
  )
}
