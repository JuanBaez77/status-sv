import { AlertTriangle, Info } from 'lucide-react'
import { useState, type ReactNode } from 'react'

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import type {
  RuntimeContainer,
  RuntimeProcess,
  RuntimeService,
  RuntimeUsage,
  ServerRuntime,
} from '@/lib/api'
import { bytes, percent, since } from '@/lib/format'
import {
  byAttention,
  containerStatus,
  processTitle,
  serviceStatus,
  sourceNotice,
  summarize,
  summaryText,
  type SourceNotice,
  type Tone,
  type UnitStatus,
} from '@/lib/runtime'
import { cn } from '@/lib/utils'

/**
 * RuntimeSections muestra lo que corre en un servidor: contenedores, servicios
 * y procesos que no maneja ninguno de los dos. Lo que tiene problemas va
 * primero, y el estado siempre dice en palabras lo que el color insinúa.
 */
export function RuntimeSections({ runtime }: { runtime: ServerRuntime }) {
  if (runtime.taken_at === null) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Contenedores y servicios</CardTitle>
          <CardDescription>
            El agent todavía no mandó lo que corre en este servidor. Si es un agent anterior al
            Sprint 2, hay que actualizarlo.
          </CardDescription>
        </CardHeader>
      </Card>
    )
  }

  return (
    // min-w-0 en cada celda: sin eso, una línea de comando larga ensancha la
    // grilla y la página se desborda en un teléfono.
    <div className="grid gap-4 lg:grid-cols-2 [&>*]:min-w-0">
      <ContainersCard runtime={runtime} />
      <ServicesCard runtime={runtime} />
      <div className="lg:col-span-2">
        <ProcessesCard runtime={runtime} />
      </div>
    </div>
  )
}

function ContainersCard({ runtime }: { runtime: ServerRuntime }) {
  const [showStopped, setShowStopped] = useState(false)
  const sorted = byAttention(runtime.containers, containerStatus, (c) => c.name)
  const summary = summarize(sorted, containerStatus)
  // Los detenidos se esconden hasta que se piden: un servidor con años de uso
  // junta contenedores viejos que tapan a los que importan.
  const visible = showStopped ? sorted : sorted.filter((c) => containerStatus(c).tone !== 'idle')

  return (
    <Section
      title="Contenedores"
      summary={summaryText(summary, 'corriendo', summary.idle === 1 ? 'detenido' : 'detenidos')}
      notice={sourceNotice(runtime.sources.docker, 'Docker')}
    >
      {visible.length > 0 && (
        <ul className="divide-y divide-border">
          {visible.map((c) => (
            <ContainerRow key={c.id} container={c} />
          ))}
        </ul>
      )}
      {summary.idle > 0 && (
        <button
          type="button"
          onClick={() => setShowStopped((value) => !value)}
          className="mt-2 text-xs text-muted-foreground underline-offset-2 hover:text-foreground hover:underline"
        >
          {showStopped
            ? 'Esconder los detenidos'
            : `Mostrar ${summary.idle} ${summary.idle === 1 ? 'detenido' : 'detenidos'}`}
        </button>
      )}
    </Section>
  )
}

function ContainerRow({ container: c }: { container: RuntimeContainer }) {
  const project = c.compose_project && `${c.compose_project}${c.compose_service ? ` / ${c.compose_service}` : ''}`
  return (
    <Row
      title={c.name}
      status={containerStatus(c)}
      usage={c.usage}
      details={[c.image, project, c.started_at && `arrancó ${since(c.started_at)}`]}
    />
  )
}

function ServicesCard({ runtime }: { runtime: ServerRuntime }) {
  const sorted = byAttention(runtime.services, serviceStatus, (s) => s.unit)
  const summary = summarize(sorted, serviceStatus)

  return (
    <Section
      title="Servicios"
      summary={summaryText(summary, summary.ok === 1 ? 'activo' : 'activos', 'sin estado')}
      notice={sourceNotice(runtime.sources.systemd, 'systemd')}
    >
      {sorted.length > 0 && (
        <ul className="divide-y divide-border">
          {sorted.map((s) => (
            <ServiceRow key={s.unit} service={s} />
          ))}
        </ul>
      )}
    </Section>
  )
}

function ServiceRow({ service: s }: { service: RuntimeService }) {
  return (
    <Row
      title={s.unit.replace(/\.service$/, '')}
      status={serviceStatus(s)}
      usage={s.usage}
      details={[
        extraSubState(s),
        s.main_pid > 0 && `PID ${s.main_pid}`,
        s.active_since && `activo ${since(s.active_since)}`,
      ]}
    />
  )
}

/**
 * obviousSubState es el subestado que no agrega nada al estado: "activo,
 * running" o "falló, failed". Solo se muestra el que dice algo más, como
 * "auto-restart" o "exited".
 */
const obviousSubState: Partial<Record<RuntimeService['active_state'], string>> = {
  active: 'running',
  failed: 'failed',
  inactive: 'dead',
}

function extraSubState(s: RuntimeService): string | false {
  return obviousSubState[s.active_state] !== s.sub_state && s.sub_state
}

function ProcessesCard({ runtime }: { runtime: ServerRuntime }) {
  return (
    <Section
      title="Procesos no gestionados"
      summary="Lo que corre fuera de Docker y de systemd: arrancado a mano, en una sesión o un tmux. Solo lectura."
      notice={sourceNotice(runtime.sources.processes, 'procesos')}
    >
      {runtime.processes.length === 0 ? (
        <p className="text-xs text-muted-foreground">Nada corriendo a mano.</p>
      ) : (
        <ul className="divide-y divide-border">
          {runtime.processes.map((p) => (
            <ProcessRow key={p.pid} process={p} />
          ))}
        </ul>
      )}
    </Section>
  )
}

function ProcessRow({ process: p }: { process: RuntimeProcess }) {
  return (
    <Row
      title={processTitle(p)}
      usage={p.usage}
      details={[p.user, p.started_at && `arrancó ${since(p.started_at)}`]}
      command={p.command}
    />
  )
}

function Section({
  title,
  summary,
  notice,
  children,
}: {
  title: string
  summary: string
  notice: SourceNotice
  children: ReactNode
}) {
  return (
    <Card className="h-full">
      <CardHeader>
        <CardTitle>{title}</CardTitle>
        <CardDescription>{summary}</CardDescription>
      </CardHeader>
      <CardContent>
        <Notice notice={notice} />
        {children}
      </CardContent>
    </Card>
  )
}

function Notice({ notice }: { notice: SourceNotice }) {
  switch (notice.kind) {
    case 'none':
      return null
    case 'error':
      return (
        <p role="alert" className="mb-2 flex gap-1.5 text-xs text-destructive">
          <AlertTriangle className="mt-0.5 size-3 shrink-0" aria-hidden />
          <span>
            No se pudo leer: {notice.text}. Lo que se ve es de {since(notice.since)}.
          </span>
        </p>
      )
    default:
      return (
        <p className="mb-2 flex gap-1.5 text-xs text-muted-foreground">
          <Info className="mt-0.5 size-3 shrink-0" aria-hidden />
          <span>{notice.text}</span>
        </p>
      )
  }
}

const toneDot: Record<Tone, string> = {
  ok: 'bg-status-online',
  warning: 'bg-status-warning',
  bad: 'bg-status-offline',
  idle: 'bg-status-unknown',
}

/**
 * Row es una unidad: nombre y estado arriba, consumo a la derecha y los
 * detalles abajo. Es una lista y no una tabla para que entre en un teléfono.
 */
function Row({
  title,
  status,
  usage,
  details,
  command,
}: {
  title: string
  status?: UnitStatus
  usage: RuntimeUsage | null
  details: (string | false | null | undefined | '')[]
  command?: string
}) {
  const shown = details.filter((d): d is string => typeof d === 'string' && d !== '')
  return (
    <li className="flex min-w-0 flex-col gap-0.5 py-2 first:pt-0 last:pb-0">
      <div className="flex items-baseline justify-between gap-3">
        <span className="min-w-0 truncate text-sm font-medium" title={title}>
          {title}
        </span>
        <span className="shrink-0 text-xs tabular-nums text-muted-foreground">
          {usage === null ? '—' : `${percent(usage.cpu_percent)} · ${bytes(usage.memory_bytes)}`}
        </span>
      </div>
      <div className="flex min-w-0 items-center gap-2 text-xs text-muted-foreground">
        {status !== undefined && (
          <span className="inline-flex shrink-0 items-center gap-1.5 text-foreground">
            <span className={cn('size-2 rounded-full', toneDot[status.tone])} aria-hidden />
            {status.label}
          </span>
        )}
        {shown.length > 0 && <span className="min-w-0 truncate">{shown.join(' · ')}</span>}
      </div>
      {command !== undefined && command !== '' && (
        <code className="block max-w-full truncate font-mono text-[11px] text-muted-foreground" title={command}>
          {command}
        </code>
      )}
    </li>
  )
}
