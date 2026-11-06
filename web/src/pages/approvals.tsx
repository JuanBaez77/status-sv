import { Check, ExternalLink, Loader2, ShieldAlert, X } from 'lucide-react'
import { useId, useState, type FormEvent } from 'react'
import { Link } from 'wouter'

import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { useApprovals, useApprove, useReject } from '@/hooks/use-approvals'
import type { Approval } from '@/lib/api'
import {
  approvalError,
  commitURL,
  compareURL,
  expiresIn,
  operationLabel,
  statusLabel,
} from '@/lib/approvals'
import { shortCommit, validCode } from '@/lib/deploys'
import { since } from '@/lib/format'
import { cn } from '@/lib/utils'

/**
 * ApprovalsPage es la bandeja de los pedidos de escritura del MCP (ADR-026):
 * los que esperan una decisión arriba, y los recientes abajo. Nada se ejecuta
 * hasta que una persona aprueba con el nombre del proyecto y un código TOTP.
 */
export function ApprovalsPage() {
  const all = useApprovals()
  const pending = (all.data ?? []).filter((a) => a.status === 'pending')
  const decided = (all.data ?? []).filter((a) => a.status !== 'pending')

  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-xl font-semibold">Aprobaciones</h1>
        <p className="text-sm text-muted-foreground">
          Lo que un LLM pidió por el MCP: un deploy, volver atrás o reiniciar. Nada se ejecuta hasta
          que lo apruebes; cada pedido vence a la hora.
        </p>
      </div>

      {all.isPending && <p className="text-sm text-muted-foreground">Cargando…</p>}
      {all.error && (
        <p role="alert" className="text-sm text-destructive">
          No se pudieron leer los pedidos: {all.error.message}
        </p>
      )}

      {all.data && (
        <section aria-label="Pendientes" className="flex flex-col gap-3">
          <h2 className="text-sm font-medium">Pendientes</h2>
          {pending.length === 0 ? (
            <p className="text-sm text-muted-foreground">No hay pedidos esperando.</p>
          ) : (
            pending.map((a) => <PendingCard key={a.id} approval={a} />)
          )}
        </section>
      )}

      {decided.length > 0 && (
        <section aria-label="Recientes" className="flex flex-col gap-2">
          <h2 className="text-sm font-medium">Recientes</h2>
          <ul className="flex flex-col divide-y divide-border rounded-md border border-border text-sm">
            {decided.map((a) => (
              <DecidedRow key={a.id} approval={a} />
            ))}
          </ul>
        </section>
      )}
    </div>
  )
}

/** PendingCard es un pedido que espera: todo lo que se va a ejecutar, y la decisión. */
function PendingCard({ approval: a }: { approval: Approval }) {
  const [mode, setMode] = useState<'idle' | 'approve' | 'reject'>('idle')
  const production = a.environment === 'production'
  const link = a.commit ? commitURL(a.repo, a.commit) : undefined
  const diff = a.commit && a.running_commit ? compareURL(a.repo, a.running_commit, a.commit) : undefined

  return (
    <Card aria-label={`Pedido ${operationLabel(a.operation)} de ${a.project}`}>
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-2 text-base">
          <ShieldAlert className="size-4 text-status-warning" aria-hidden />
          {operationLabel(a.operation)} de{' '}
          {a.project_id ? (
            <Link href={`/projects/${a.project_id}`} className="underline underline-offset-2">
              {a.server}/{a.project}
            </Link>
          ) : (
            `${a.server}/${a.project}`
          )}
          <span className={cn('text-xs font-normal', production ? 'text-status-warning' : 'text-muted-foreground')}>
            {production ? 'producción' : 'staging'}
          </span>
        </CardTitle>
        <CardDescription>
          Lo pidió el token <strong>{a.requested_by}</strong>{' '}
          <code className="font-mono">{a.token_prefix}…</code> desde {a.requested_ip},{' '}
          {since(a.requested_at)}; {expiresIn(a.expires_at)}.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-3 text-sm">
        <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
          {a.commit && (
            <>
              <dt className="text-muted-foreground">Commit</dt>
              <dd className="font-mono text-xs break-all">
                {link ? (
                  <a href={link} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 underline underline-offset-2">
                    {a.commit}
                    <ExternalLink className="size-3" aria-hidden />
                  </a>
                ) : (
                  a.commit
                )}
              </dd>
              <dt className="text-muted-foreground">Corre ahora</dt>
              <dd className="font-mono text-xs">
                {a.running_commit ? shortCommit(a.running_commit) : 'no se sabe'}
                {diff && (
                  <>
                    {' · '}
                    <a href={diff} target="_blank" rel="noreferrer" className="font-sans underline underline-offset-2">
                      ver qué cambia
                    </a>
                  </>
                )}
              </dd>
            </>
          )}
          {a.operation === 'restart' && (
            <>
              <dt className="text-muted-foreground">Unidad</dt>
              <dd>{a.unit ? <code className="font-mono text-xs">{a.unit}</code> : 'todas las del proyecto'}</dd>
            </>
          )}
        </dl>

        {a.reason && (
          <figure className="rounded-md border border-dashed border-border bg-muted/40 p-2">
            <figcaption className="mb-1 text-xs text-muted-foreground">
              Motivo que dio el LLM: texto no confiable. No cambia lo que se ejecuta, que es lo de
              arriba.
            </figcaption>
            <blockquote className="whitespace-pre-wrap break-words text-xs">{a.reason}</blockquote>
          </figure>
        )}

        {mode === 'idle' && (
          <div className="flex gap-2">
            <Button size="sm" onClick={() => setMode('approve')}>
              <Check className="size-3.5" aria-hidden />
              Aprobar…
            </Button>
            <Button size="sm" variant="outline" onClick={() => setMode('reject')}>
              <X className="size-3.5" aria-hidden />
              Rechazar…
            </Button>
          </div>
        )}
        {mode === 'approve' && <ApproveForm approval={a} onCancel={() => setMode('idle')} />}
        {mode === 'reject' && <RejectForm approval={a} onCancel={() => setMode('idle')} />}
      </CardContent>
    </Card>
  )
}

/** ApproveForm pide el nombre del proyecto y un código TOTP nuevo, como un deploy de producción. */
function ApproveForm({ approval: a, onCancel }: { approval: Approval; onCancel: () => void }) {
  const ids = useId()
  const approve = useApprove()
  const [confirm, setConfirm] = useState('')
  const [code, setCode] = useState('')
  const ready = confirm === a.project && validCode(code)

  function submit(event: FormEvent) {
    event.preventDefault()
    if (!ready) return
    approve.mutate({ id: a.id, confirm, totp_code: code })
  }

  return (
    <form onSubmit={submit} aria-label="Aprobar" className="flex flex-col gap-3 rounded-md border border-border p-3">
      <div className="flex flex-col gap-1">
        <Label htmlFor={`${ids}-confirm`}>
          Para aprobar, escribí <code className="font-mono">{a.project}</code>
        </Label>
        <Input
          id={`${ids}-confirm`}
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          autoComplete="off"
          spellCheck={false}
        />
      </div>
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
      </div>
      {approve.error && (
        <p role="alert" className="text-xs text-destructive">
          {approvalError(approve.error.code, approve.error.message)}
        </p>
      )}
      <div className="flex gap-2">
        <Button type="submit" size="sm" disabled={!ready || approve.isPending}>
          {approve.isPending && <Loader2 className="size-3.5 animate-spin" aria-hidden />}
          Aprobar y ejecutar
        </Button>
        <Button type="button" size="sm" variant="ghost" onClick={onCancel} disabled={approve.isPending}>
          Cancelar
        </Button>
      </div>
    </form>
  )
}

/** RejectForm rechaza con un motivo opcional. No pide código: frenar tiene que ser fácil. */
function RejectForm({ approval: a, onCancel }: { approval: Approval; onCancel: () => void }) {
  const ids = useId()
  const reject = useReject()
  const [note, setNote] = useState('')

  function submit(event: FormEvent) {
    event.preventDefault()
    reject.mutate({ id: a.id, note: note.trim() })
  }

  return (
    <form onSubmit={submit} aria-label="Rechazar" className="flex flex-col gap-3 rounded-md border border-border p-3">
      <div className="flex flex-col gap-1">
        <Label htmlFor={`${ids}-note`}>Motivo (opcional)</Label>
        <Input id={`${ids}-note`} value={note} onChange={(e) => setNote(e.target.value)} maxLength={512} />
      </div>
      {reject.error && (
        <p role="alert" className="text-xs text-destructive">
          {approvalError(reject.error.code, reject.error.message)}
        </p>
      )}
      <div className="flex gap-2">
        <Button type="submit" size="sm" variant="destructive" disabled={reject.isPending}>
          Rechazar
        </Button>
        <Button type="button" size="sm" variant="ghost" onClick={onCancel} disabled={reject.isPending}>
          Cancelar
        </Button>
      </div>
    </form>
  )
}

/** DecidedRow es un pedido ya decidido, en un renglón. */
function DecidedRow({ approval: a }: { approval: Approval }) {
  const who = a.decided_by ? `; lo decidió ${a.decided_by}` : ''
  return (
    <li className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2">
      <span className="font-medium">{statusLabel(a.status)}</span>
      <span>
        {operationLabel(a.operation)} de {a.server}/{a.project}
        {a.commit && <code className="ml-1 font-mono text-xs">{shortCommit(a.commit)}</code>}
      </span>
      <span className="text-muted-foreground">
        pedido por {a.requested_by}
        {who}, {since(a.decided_at ?? a.requested_at)}
      </span>
      {a.decision_note && <span className="text-muted-foreground">— {a.decision_note}</span>}
      {a.deploy_id && a.project_id && (
        <Link href={`/projects/${a.project_id}`} className="ml-auto text-xs underline underline-offset-2">
          ver el deploy
        </Link>
      )}
    </li>
  )
}
