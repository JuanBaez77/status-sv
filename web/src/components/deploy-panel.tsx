import {
  CheckCircle2,
  ChevronDown,
  ChevronRight,
  CircleDashed,
  Loader2,
  MinusCircle,
  Play,
  Rocket,
  Undo2,
  XCircle,
} from 'lucide-react'
import { useId, useState, type FormEvent } from 'react'
import { Link } from 'wouter'

import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { useDeploy, useDeployHistory, useStartDeploy } from '@/hooks/use-deploys'
import type { DeployDetail, DeployKind, DeployStep, DeploySummary, ProjectDetail } from '@/lib/api'
import { auditLink } from '@/lib/audit'
import {
  active,
  codeText,
  duration,
  kindLabel,
  neverRan,
  requestError,
  shortCommit,
  statusLabel,
  statusTone,
  stepLabel,
  validCode,
  validCommit,
} from '@/lib/deploys'
import { since } from '@/lib/format'
import { cn } from '@/lib/utils'

/** Runtime es cómo corre un proyecto: con Docker Compose o como servicio de systemd. */
type Runtime = ProjectDetail['runtime']

/** historyShown es cuántos deploys se muestran de la historia. */
const historyShown = 10

/**
 * DeploysCard es el deploy de un proyecto desde el panel (ADR-017): pedir un
 * deploy o un rollback con confirmación, ver el que está en curso paso por
 * paso y la historia. Si el proyecto todavía no corre en el servidor, ofrece
 * el primer deploy (ADR-021).
 */
export function DeploysCard({ project }: { project: ProjectDetail }) {
  const history = useDeployHistory(project.id, project.server_id)
  const [form, setForm] = useState<DeployKind | null>(null)
  const [opened, setOpened] = useState<string | null>(null)

  const list = history.data?.deploys ?? []
  const current = list.find((d) => active(d.status))
  const docker = project.runtime === 'docker'
  // Recién con la historia a la vista se sabe si nunca se desplegó.
  const first = history.data !== undefined && neverRan(project, list)

  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="flex flex-col gap-1.5">
            <CardTitle>Deploys</CardTitle>
            <CardDescription>
              {docker
                ? `Trae la rama ${project.branch}, compila y levanta con docker compose. Si no queda sano, vuelve solo a lo anterior.`
                : `Trae la rama ${project.branch}, corre los pasos declarados en el servidor y reinicia ${unitsText(project.declared_units)}. Si no queda sano, vuelve solo a lo anterior.`}
            </CardDescription>
          </div>
          {form === null && (
            <div className="flex flex-wrap gap-2">
              {first && (
                <Button
                  size="sm"
                  onClick={() => setForm('bootstrap')}
                  disabled={current !== undefined}
                >
                  <Play className="size-3.5" aria-hidden />
                  Primer deploy
                </Button>
              )}
              <Button
                size="sm"
                variant={first ? 'outline' : 'default'}
                onClick={() => setForm('deploy')}
                disabled={current !== undefined}
              >
                <Rocket className="size-3.5" aria-hidden />
                Desplegar
              </Button>
              <Button
                size="sm"
                variant="outline"
                onClick={() => setForm('rollback')}
                disabled={current !== undefined}
              >
                <Undo2 className="size-3.5" aria-hidden />
                Volver atrás
              </Button>
            </div>
          )}
        </div>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {first && form === null && (
          <p className="text-xs text-muted-foreground">
            Este proyecto todavía no corre en el servidor. El primer deploy clona la rama en{' '}
            <code className="font-mono">{project.workdir}</code>, lo prepara y lo levanta.
          </p>
        )}
        {form !== null && (
          <DeployForm
            project={project}
            kind={form}
            onDone={(deploy) => {
              setForm(null)
              if (deploy) setOpened(deploy.id)
            }}
          />
        )}

        {current && <DeployProgress id={current.id} runtime={project.runtime} />}

        {history.error ? (
          <p className="text-xs text-destructive">
            No se pudo traer la historia: {history.error.message}
          </p>
        ) : list.length === 0 ? (
          history.isPending ? null : (
            <p className="text-xs text-muted-foreground">Todavía no se desplegó desde el panel.</p>
          )
        ) : (
          <ul className="divide-y divide-border" aria-label="Historia de deploys">
            {list.slice(0, historyShown).map((d) => (
              <HistoryRow
                key={d.id}
                deploy={d}
                runtime={project.runtime}
                open={opened === d.id && d.id !== current?.id}
                onToggle={() => setOpened(opened === d.id ? null : d.id)}
                isCurrent={d.id === current?.id}
              />
            ))}
          </ul>
        )}

        <p className="text-xs text-muted-foreground">
          Quién pidió cada deploy y cómo terminó queda en la{' '}
          <Link
            href={auditLink('project', project.id)}
            className="text-foreground underline underline-offset-2"
          >
            auditoría de este proyecto
          </Link>
          .
        </p>
      </CardContent>
    </Card>
  )
}

/** unitsText nombra las unidades de un proyecto de systemd en una frase. */
function unitsText(units: string[] | undefined): string {
  if (!units || units.length === 0) return 'sus servicios'
  if (units.length === 1) return units[0]
  return `${units.slice(0, -1).join(', ')} y ${units[units.length - 1]}`
}

/**
 * DeployForm pide la confirmación: el nombre del proyecto escrito a mano y, en
 * producción, un código nuevo de la app (ADR-017). Un primer deploy pide el
 * código siempre (ADR-021).
 */
function DeployForm({
  project,
  kind,
  onDone,
}: {
  project: ProjectDetail
  kind: DeployKind
  onDone: (deploy: DeployDetail | null) => void
}) {
  const ids = useId()
  const start = useStartDeploy(project.id)
  const [commit, setCommit] = useState('')
  const [confirm, setConfirm] = useState('')
  const [code, setCode] = useState('')
  const production = project.environment === 'production'
  const needsCode = production || kind === 'bootstrap'
  const title = formTitles[kind]

  const commitOK = commit === '' || validCommit(commit)
  const ready = confirm === project.name && commitOK && (!needsCode || validCode(code))

  function submit(event: FormEvent) {
    event.preventDefault()
    if (!ready) return
    start.mutate(
      {
        kind,
        request: {
          confirm,
          ...(kind === 'deploy' && commit !== '' ? { commit } : {}),
          ...(needsCode ? { totp_code: code } : {}),
        },
      },
      { onSuccess: (deploy) => onDone(deploy) },
    )
  }

  return (
    <form
      onSubmit={submit}
      className="flex flex-col gap-3 rounded-md border border-border p-3"
      aria-label={title}
    >
      <p className="text-sm">
        {kind === 'bootstrap' ? (
          <>
            Primer deploy de <strong>{project.name}</strong> en {project.server_name}
            {production && <span className="text-status-warning"> (producción)</span>}: clona la
            rama {project.branch} en <code className="font-mono">{project.workdir}</code>, lo
            prepara y lo levanta. Si no queda sano, baja lo que levantó; el código queda en el
            servidor.
          </>
        ) : kind === 'deploy' ? (
          <>
            Desplegar <strong>{project.name}</strong> en {project.server_name}
            {production && <span className="text-status-warning"> (producción)</span>}.
          </>
        ) : project.runtime === 'systemd' ? (
          <>
            Volver <strong>{project.name}</strong> al commit bueno anterior. Vuelve el código, se
            corren de nuevo los pasos que lo preparan y se reinicia el servicio;{' '}
            <strong>
              los cambios en la base (por ejemplo, las migraciones de un paso previo) no se deshacen
            </strong>
            .
          </>
        ) : (
          <>
            Volver <strong>{project.name}</strong> al estado bueno anterior, sin compilar. Vuelven
            el código y las imágenes;{' '}
            <strong>
              los cambios en la base (por ejemplo, las migraciones de un paso previo) no se deshacen
            </strong>
            .
          </>
        )}
      </p>

      {kind === 'deploy' && (
        <div className="flex flex-col gap-1">
          <Label htmlFor={`${ids}-commit`}>Commit (opcional)</Label>
          <Input
            id={`${ids}-commit`}
            value={commit}
            onChange={(e) => setCommit(e.target.value.trim())}
            placeholder={`vacío: la punta de ${project.branch}`}
            className="font-mono text-xs"
            autoComplete="off"
            spellCheck={false}
            aria-invalid={!commitOK}
          />
          {!commitOK && (
            <p className="text-xs text-destructive">
              Tiene que ser un SHA completo, de 40 caracteres.
            </p>
          )}
        </div>
      )}

      <div className="flex flex-col gap-1">
        <Label htmlFor={`${ids}-confirm`}>
          Para confirmar, escribí <code className="font-mono">{project.name}</code>
        </Label>
        <Input
          id={`${ids}-confirm`}
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          autoComplete="off"
          spellCheck={false}
        />
      </div>

      {needsCode && (
        <div className="flex flex-col gap-1">
          <Label htmlFor={`${ids}-code`}>Código de la app de autenticación</Label>
          <Input
            id={`${ids}-code`}
            value={code}
            onChange={(e) => setCode(e.target.value.replace(/\D/g, '').slice(0, 6))}
            inputMode="numeric"
            autoComplete="one-time-code"
            className="w-32 font-mono tracking-widest"
          />
          <p className="text-xs text-muted-foreground">
            {!production && 'Un primer deploy lo pide siempre. '}
            Uno nuevo: el que usaste para entrar no sirve dos veces.
          </p>
        </div>
      )}

      {start.error && (
        <p role="alert" className="text-xs text-destructive">
          {requestError(start.error.code, start.error.message)}
        </p>
      )}

      <div className="flex gap-2">
        <Button type="submit" size="sm" disabled={!ready || start.isPending}>
          {start.isPending && <Loader2 className="size-3.5 animate-spin" aria-hidden />}
          {title}
        </Button>
        <Button
          type="button"
          size="sm"
          variant="ghost"
          onClick={() => onDone(null)}
          disabled={start.isPending}
        >
          Cancelar
        </Button>
      </div>
    </form>
  )
}

/** formTitles es cómo se llama cada pedido, en el formulario y en su botón. */
const formTitles: Record<DeployKind, string> = {
  deploy: 'Desplegar',
  rollback: 'Volver atrás',
  bootstrap: 'Primer deploy',
  // Un reinicio se pide por MCP y se aprueba en Aprobaciones (ADR-026): el
  // panel no tiene un formulario para pedirlo.
  restart: 'Reiniciar',
}

const toneText: Record<ReturnType<typeof statusTone>, string> = {
  ok: 'text-status-online',
  warn: 'text-status-warning',
  bad: 'text-destructive',
  busy: 'text-muted-foreground',
}

function HistoryRow({
  deploy: d,
  runtime,
  open,
  onToggle,
  isCurrent,
}: {
  deploy: DeploySummary
  runtime: Runtime
  open: boolean
  onToggle: () => void
  isCurrent: boolean
}) {
  const panel = useId()
  return (
    <li className="flex flex-col gap-1 py-2 text-sm">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <span className={cn('text-xs font-medium', toneText[statusTone(d)])}>
          {statusLabel(d.status, d.kind)}
        </span>
        <span className="text-xs text-muted-foreground">{kindLabel(d.kind)}</span>
        <code className="font-mono text-xs">{shortCommit(d.commit ?? d.requested_commit)}</code>
        <span className="text-xs text-muted-foreground">
          por {d.requested_by} · {since(d.requested_at)}
          {d.finished_at && ` · ${duration(d.started_at ?? d.requested_at, d.finished_at)}`}
        </span>
        {!isCurrent && (
          <button
            type="button"
            onClick={onToggle}
            aria-expanded={open}
            aria-controls={panel}
            className="ml-auto inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
          >
            {open ? (
              <ChevronDown className="size-3" aria-hidden />
            ) : (
              <ChevronRight className="size-3" aria-hidden />
            )}
            Pasos
          </button>
        )}
      </div>
      {d.code && (
        <p className="text-xs text-muted-foreground">
          {codeText(d.code)}
          {d.message ? ` — ${d.message}` : ''}
        </p>
      )}
      {open && (
        <div id={panel}>
          <DeployProgress id={d.id} runtime={runtime} />
        </div>
      )}
    </li>
  )
}

/**
 * DeployProgress muestra los pasos de un deploy, en vivo si está en curso.
 * runtime dice cómo corre el proyecto: cambia cómo se nombran algunos pasos.
 */
export function DeployProgress({ id, runtime = 'docker' }: { id: string; runtime?: Runtime }) {
  const { data: d, error, isPending } = useDeploy(id)
  if (isPending) return <p className="text-xs text-muted-foreground">Cargando los pasos…</p>
  if (error) return <p className="text-xs text-destructive">{error.message}</p>

  return (
    <div className="flex flex-col gap-2 rounded-md bg-muted/40 p-3" aria-live="polite">
      <p className="text-xs text-muted-foreground">
        {d.status === 'pending'
          ? 'Esperando que el agent empiece…'
          : d.previous_commit
            ? `${shortCommit(d.previous_commit)} → ${shortCommit(d.commit)}`
            : shortCommit(d.commit)}
      </p>
      <ol className="flex flex-col gap-1">
        {d.steps.map((step, i) => (
          <StepRow
            key={`${step.name}-${step.service ?? ''}-${i}`}
            step={step}
            runtime={runtime}
            kind={d.kind}
          />
        ))}
      </ol>
    </div>
  )
}

function StepRow({
  step,
  runtime,
  kind,
}: {
  step: DeployStep
  runtime: Runtime
  kind: DeployKind
}) {
  // La salida se muestra abierta en el paso en curso y en el que falló.
  const [open, setOpen] = useState(step.status === 'running' || step.status === 'failed')
  const hasOutput = (step.output?.length ?? 0) > 0
  return (
    <li className="flex flex-col gap-1 text-xs">
      <div className="flex items-center gap-2">
        <StepIcon status={step.status} />
        <span className="font-medium">{stepLabel(step, runtime, kind)}</span>
        {step.detail && (
          <span className="min-w-0 truncate text-muted-foreground">{step.detail}</span>
        )}
        <span className="ml-auto shrink-0 text-muted-foreground">
          {step.status !== 'skipped' && duration(step.started_at, step.finished_at)}
        </span>
        {hasOutput && (
          <button
            type="button"
            onClick={() => setOpen(!open)}
            aria-expanded={open}
            className="shrink-0 text-muted-foreground hover:text-foreground"
          >
            {open ? 'Ocultar salida' : 'Salida'}
          </button>
        )}
      </div>
      {open && hasOutput && (
        <pre className="max-h-48 overflow-auto rounded-md bg-muted p-2 font-mono text-[11px] leading-relaxed whitespace-pre-wrap break-all">
          {step.output?.join('\n')}
        </pre>
      )}
    </li>
  )
}

function StepIcon({ status }: { status: DeployStep['status'] }) {
  switch (status) {
    case 'ok':
      return <CheckCircle2 className="size-3.5 shrink-0 text-status-online" aria-label="bien" />
    case 'failed':
      return <XCircle className="size-3.5 shrink-0 text-destructive" aria-label="falló" />
    case 'skipped':
      return (
        <MinusCircle
          className="size-3.5 shrink-0 text-muted-foreground"
          aria-label="no hizo falta"
        />
      )
    case 'running':
      return (
        <Loader2
          className="size-3.5 shrink-0 animate-spin text-muted-foreground"
          aria-label="en curso"
        />
      )
    default:
      return <CircleDashed className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
  }
}
