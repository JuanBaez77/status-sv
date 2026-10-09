import { ArrowLeft, Plus } from 'lucide-react'
import { useMemo, type ReactNode } from 'react'
import { Link, useSearchParams } from 'wouter'

import { Chart } from '@/components/chart'
import { LiveIndicator } from '@/components/live-indicator'
import { RangePicker } from '@/components/range-picker'
import { RuntimeSections } from '@/components/runtime-sections'
import { StatusBadge } from '@/components/status-badge'
import { UsageBar } from '@/components/usage-bar'
import { buttonVariants } from '@/components/ui/button-variants'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useLiveSamples } from '@/hooks/use-live-samples'
import { useServerMetrics } from '@/hooks/use-server-metrics'
import { useProjects } from '@/hooks/use-projects'
import { useServerProposals } from '@/hooks/use-proposals'
import { useServerRuntime } from '@/hooks/use-server-runtime'
import { useServers } from '@/hooks/use-servers'
import type { Resolution, ServerMetrics } from '@/lib/api'
import { bytes, load, percent, perSecond, ratio, since } from '@/lib/format'
import { isOpen, statusLabel as proposalStatusLabel } from '@/lib/proposals'
import { defaultRange, isRangeKey } from '@/lib/ranges'
import {
  cpuChart,
  currentReading,
  diskChart,
  loadChart,
  memoryChart,
  mergeLive,
  networkChart,
} from '@/lib/series'
import { cn } from '@/lib/utils'
import { ProjectCard } from '@/components/project-card'

/**
 * ServerPage es la vista de un servidor: los números de ahora arriba y los
 * gráficos del rango elegido abajo.
 *
 * Los datos vienen de dos lados. Las series salen de la API REST cada vez que
 * cambia el rango; las muestras nuevas llegan por el canal en vivo y se pegan
 * al final, así el gráfico avanza sin volver a pedir todo cada diez segundos.
 */
export function ServerPage({ id }: { id: string }) {
  const [params, setParams] = useSearchParams()
  // El rango va en la dirección: así un enlace al panel lleva puesto lo que
  // estaba mirando quien lo mandó.
  const asked = params.get('rango')
  const range = isRangeKey(asked) ? asked : defaultRange

  const list = useServers()
  const metrics = useServerMetrics(id, range)
  const live = useLiveSamples(id)
  const runtime = useServerRuntime(id)

  const server = list.data?.find((candidate) => candidate.id === id)

  const series = useMemo(
    () => (metrics.data === undefined ? undefined : mergeLive(metrics.data, live.samples)),
    [metrics.data, live.samples],
  )
  const reading = useMemo(() => currentReading(series, live.samples), [series, live.samples])

  if (list.isPending) {
    return <p className="text-sm text-muted-foreground">Cargando el servidor…</p>
  }

  if (list.error !== null) {
    return (
      <Message title="No se pudo traer el servidor" description={list.error.message}>
        <BackLink />
      </Message>
    )
  }

  if (server === undefined) {
    return (
      <Message
        title="No existe ese servidor"
        description="Puede que lo hayan dado de baja, o que el enlace esté mal."
      >
        <BackLink />
      </Message>
    )
  }

  const sample = reading.sample
  const rx = reading.networks.reduce((total, entry) => total + entry.point.rx_bytes_per_sec, 0)
  const tx = reading.networks.reduce((total, entry) => total + entry.point.tx_bytes_per_sec, 0)

  return (
    <div className="flex flex-col gap-4">
      <header className="flex flex-col gap-3">
        <BackLink />

        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <h1 className="truncate text-lg font-medium">{server.name}</h1>
            <p className="truncate text-xs text-muted-foreground">
              {server.hostname} · agent {server.agent_version || '—'} · último latido{' '}
              {since(server.last_seen_at)}
            </p>
          </div>
          <div className="flex items-center gap-3">
            <StatusBadge status={server.status} />
            <LiveIndicator status={live.status} />
          </div>
        </div>

        <div className="flex flex-wrap items-center justify-between gap-2">
          <RangePicker
            value={range}
            onChange={(next) => setParams({ rango: next }, { replace: true })}
          />
          <p className="text-xs text-muted-foreground">
            {resolutionNote(series?.resolution)}
          </p>
        </div>
      </header>

      {sample === null ? (
        <Message
          title="Sin métricas todavía"
          description="Este servidor está dado de alta pero el agent no mandó ninguna muestra."
        />
      ) : (
        // data-sample-ts es la hora de la muestra que se está mostrando: con
        // eso, la medición de CI (deploy/metas) ve cuándo se dibuja cada una.
        <ul className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4" data-sample-ts={sample.ts}>
          <Stat
            label="CPU"
            value={percent(sample.cpu_used_percent)}
            detail={`${sample.cpu_cores} núcleos · espera de E/S ${percent(sample.cpu_iowait_percent)}`}
          />
          <Stat
            label="Memoria"
            value={ratio(sample.mem_used_bytes, sample.mem_total_bytes)}
            detail={`${bytes(sample.mem_used_bytes)} de ${bytes(sample.mem_total_bytes)}`}
          />
          <Stat
            label="Carga"
            value={load(sample.load1)}
            detail={`5 min ${load(sample.load5)} · 15 min ${load(sample.load15)}`}
          />
          <Stat
            label="Red (entrada)"
            value={perSecond(rx)}
            detail={`salida ${perSecond(tx)}`}
          />
        </ul>
      )}

      {reading.disks.length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle>Discos</CardTitle>
            <CardDescription>Cuánto ocupa cada punto de montaje ahora mismo.</CardDescription>
          </CardHeader>
          <CardContent className="grid gap-3 sm:grid-cols-2">
            {reading.disks.map(({ mountpoint, point }) => (
              <UsageBar
                key={mountpoint}
                label={mountpoint}
                value={point.total_bytes > 0 ? (point.used_bytes / point.total_bytes) * 100 : 0}
                detail={`quedan ${bytes(point.available_bytes)} de ${bytes(point.total_bytes)}`}
              />
            ))}
          </CardContent>
        </Card>
      )}

      <ServerProjects serverID={id} />

      {runtime.data !== undefined && <RuntimeSections runtime={runtime.data} />}
      {runtime.error !== null && (
        <Message title="No se pudo traer lo que corre en el servidor" description={runtime.error.message} />
      )}

      <Charts series={series} loading={metrics.isPending} stale={metrics.isPlaceholderData} />

      {metrics.error !== null && (
        <Message title="No se pudieron traer las métricas" description={metrics.error.message} />
      )}
    </div>
  )
}

function Charts({
  series,
  loading,
  stale,
}: {
  series: ServerMetrics | undefined
  loading: boolean
  stale: boolean
}) {
  if (loading || series === undefined) {
    return <p className="text-sm text-muted-foreground">Cargando métricas…</p>
  }

  if (series.samples.length === 0) {
    return (
      <Message
        title="No hay métricas en este rango"
        description="Probá con un rango más largo: el crudo se guarda 48 horas y el promedio por minuto, 30 días."
      />
    )
  }

  return <ChartGrid series={series} stale={stale} />
}

function ChartGrid({ series, stale }: { series: ServerMetrics; stale: boolean }) {
  const cpu = useMemo(() => cpuChart(series), [series])
  const memory = useMemo(() => memoryChart(series), [series])
  const loadData = useMemo(() => loadChart(series), [series])
  const disks = useMemo(() => diskChart(series), [series])
  const networks = useMemo(
    () =>
      series.networks.map((entry) => ({
        name: entry.interface,
        data: networkChart(series, entry),
      })),
    [series],
  )

  return (
    <div className={cn('grid gap-4 lg:grid-cols-2', stale && 'opacity-60')}>
      <ChartCard title="CPU" description="Porcentaje de uso del procesador.">
        <Chart label="Uso de CPU" data={cpu} format={percent} max={100} />
      </ChartCard>

      <ChartCard title="Memoria" description="Memoria ocupada, sin contar cachés.">
        <Chart label="Memoria en uso" data={memory} format={bytes} />
      </ChartCard>

      <ChartCard title="Carga" description="Procesos esperando turno, promediados por el kernel.">
        <Chart label="Carga del sistema" data={loadData} format={load} />
      </ChartCard>

      {disks.lines.length > 0 && (
        <ChartCard title="Discos" description="Porcentaje ocupado de cada punto de montaje.">
          <Chart label="Uso de disco" data={disks} format={percent} max={100} />
        </ChartCard>
      )}

      {networks.map((entry) => (
        <ChartCard
          key={entry.name}
          title={`Red · ${entry.name}`}
          description="Lo que entra y lo que sale, por segundo."
        >
          <Chart label={`Tráfico de ${entry.name}`} data={entry.data} format={perSecond} />
        </ChartCard>
      ))}
    </div>
  )
}

function ChartCard({
  title,
  description,
  children,
}: {
  title: string
  description: string
  children: ReactNode
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
        <CardDescription>{description}</CardDescription>
      </CardHeader>
      <CardContent>{children}</CardContent>
    </Card>
  )
}

/** resolutionNote explica qué es cada punto de los gráficos. */
function resolutionNote(resolution: Resolution | undefined): string {
  switch (resolution) {
    case '1m':
      return 'Promedio por minuto; la línea punteada es el pico.'
    case '1h':
      return 'Promedio por hora; las líneas punteadas son el pico y el mínimo.'
    default:
      return 'Cada punto es una muestra del agent.'
  }
}

function Stat({ label, value, detail }: { label: string; value: string; detail: string }) {
  return (
    <li>
      <Card className="h-full">
        <CardHeader>
          <CardDescription>{label}</CardDescription>
          <p className="text-xl font-medium tabular-nums">{value}</p>
        </CardHeader>
        <CardContent>
          <p className="text-xs text-muted-foreground">{detail}</p>
        </CardContent>
      </Card>
    </li>
  )
}

function Message({
  title,
  description,
  children,
}: {
  title: string
  description?: string
  children?: ReactNode
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
        {description !== undefined && <CardDescription>{description}</CardDescription>}
      </CardHeader>
      {children !== undefined && <CardContent>{children}</CardContent>}
    </Card>
  )
}

function BackLink() {
  return (
    <Link
      href="/"
      className="inline-flex w-fit items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
    >
      <ArrowLeft className="size-3" aria-hidden />
      Servidores
    </Link>
  )
}

/**
 * ServerProjects son los proyectos declarados en este servidor. No se muestra
 * nada si no hay: la mayoría de los servidores todavía no tiene.
 */
function ServerProjects({ serverID }: { serverID: string }) {
  const { data } = useProjects()
  const proposals = useServerProposals(serverID)
  const mine = data?.projects.filter((p) => p.server_id === serverID) ?? []
  const open = proposals.data?.filter((p) => isOpen(p.status)) ?? []
  return (
    <section aria-labelledby="server-projects" className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 id="server-projects" className="text-sm font-medium">
          Proyectos
        </h2>
        <Link href={`/servers/${serverID}/new-project`} className={buttonVariants({ variant: 'outline', size: 'sm' })}>
          <Plus className="size-3.5" aria-hidden />
          Nuevo proyecto
        </Link>
      </div>
      {open.length > 0 && (
        <ul aria-label="Propuestas abiertas" className="flex flex-col gap-1">
          {open.map((p) => (
            <li key={p.id} className="text-xs">
              <Link href={`/proposals/${p.id}`} className="font-medium underline-offset-2 hover:underline">
                {p.name}
              </Link>{' '}
              <span className="text-muted-foreground">
                · propuesto {since(p.created_at)} · {proposalStatusLabel(p.status).toLowerCase()}
              </span>
            </li>
          ))}
        </ul>
      )}
      {mine.length > 0 && (
        <ul className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {mine.map((p) => (
            <li key={p.id}>
              <ProjectCard project={p} />
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
