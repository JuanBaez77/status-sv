import { ArrowLeft } from 'lucide-react'
import { Link } from 'wouter'

import { CommandBlock } from '@/components/copy-button'
import { PreviewSections } from '@/components/proposal-preview'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useProjects } from '@/hooks/use-projects'
import { useProposal, useWithdrawProposal } from '@/hooks/use-proposals'
import { useServers } from '@/hooks/use-servers'
import type { ProposalDetail } from '@/lib/api'
import { since } from '@/lib/format'
import { environmentLabel } from '@/lib/projects'
import { isOpen, statusLabel, statusTone } from '@/lib/proposals'
import { cn } from '@/lib/utils'

const toneText: Record<ReturnType<typeof statusTone>, string> = {
  ok: 'text-status-online',
  busy: 'text-muted-foreground',
  bad: 'text-destructive',
  neutral: 'text-muted-foreground',
}

/**
 * ProposalPage muestra una propuesta de proyecto nuevo: en qué anda, el
 * comando para aceptarla en el servidor y lo que va a escribir. Se mantiene
 * al día sola: cuando el agent la recibe o alguien la acepta, cambia.
 */
export function ProposalPage({ id }: { id: string }) {
  const { data: p, isPending, error } = useProposal(id)
  const servers = useServers()

  if (isPending) {
    return <p className="text-sm text-muted-foreground">Cargando la propuesta…</p>
  }
  if (error) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>{error.status === 404 ? 'No existe esa propuesta' : 'No se pudo traer la propuesta'}</CardTitle>
          <CardDescription>{error.status === 404 ? 'Puede que el enlace esté mal.' : error.message}</CardDescription>
        </CardHeader>
      </Card>
    )
  }

  const server = servers.data?.find((s) => s.id === p.server_id)
  const serverName = server?.name ?? 'el servidor'

  return (
    <section className="flex flex-col gap-4">
      <Link
        href={`/servers/${p.server_id}`}
        className="inline-flex w-fit items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
      >
        <ArrowLeft className="size-3" aria-hidden />
        {server?.name ?? 'Servidor'}
      </Link>
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <h1 className="text-lg font-medium">Propuesta: {p.name}</h1>
          <p className="text-xs text-muted-foreground">
            {serverName} · {environmentLabel(p.environment)} · {p.runtime} · la propuso {p.created_by}{' '}
            {since(p.created_at)}
          </p>
        </div>
        <span className={cn('text-sm font-medium', toneText[statusTone(p.status)])}>
          {statusLabel(p.status)}
        </span>
      </header>

      <Next proposal={p} serverName={serverName} />

      <PreviewSections
        preview={p.preview}
        intro="Con las plantillas de fábrica. Si el servidor tiene las suyas, el comando de aceptación muestra lo que escribe de verdad antes de hacer nada."
      />
    </section>
  )
}

/** Next dice qué sigue según en qué anda la propuesta. */
function Next({ proposal: p, serverName }: { proposal: ProposalDetail; serverName: string }) {
  const withdraw = useWithdrawProposal(p.id)
  const projects = useProjects()

  if (isOpen(p.status)) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Para aceptarla</CardTitle>
          <CardDescription>
            {p.status === 'pending'
              ? `Todavía no llegó a ${serverName}: le llega cuando el agent se conecte.`
              : `Ya la tiene el agent de ${serverName}.`}{' '}
            Una persona con acceso root la acepta ahí. El comando muestra todo lo que va a hacer,
            pide los valores de las variables sin mostrarlos y no cambia nada hasta que se escribe
            el nombre del proyecto.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <CommandBlock command={p.command} label="Copiar el comando de aceptación" />
          <p className="text-xs text-muted-foreground">
            Vence {until(p.expires_at)} si nadie la acepta. Una propuesta nueva con el mismo
            nombre la reemplaza.
          </p>
          {withdraw.error && (
            <p role="alert" className="text-sm text-destructive">
              {withdraw.error.message}
            </p>
          )}
          <div>
            <Button variant="outline" size="sm" onClick={() => withdraw.mutate()} disabled={withdraw.isPending}>
              {withdraw.isPending ? 'Retirando…' : 'Retirar la propuesta'}
            </Button>
          </div>
        </CardContent>
      </Card>
    )
  }

  if (p.status === 'accepted') {
    const project = projects.data?.projects.find((x) => x.server_id === p.server_id && x.name === p.name)
    return (
      <Card>
        <CardHeader>
          <CardTitle>Aceptada en {serverName}</CardTitle>
          <CardDescription>
            {p.message === 'declarada distinta de la propuesta'
              ? 'Lo que quedó declarado no es exactamente lo propuesto: alguien lo cambió en el servidor.'
              : 'Quedó declarada tal cual se propuso.'}{' '}
            Lo que sigue es el primer deploy, desde el proyecto.
          </CardDescription>
        </CardHeader>
        {project && (
          <CardContent>
            <Link href={`/projects/${project.id}`} className="text-sm text-primary underline">
              Ir al proyecto
            </Link>
          </CardContent>
        )}
      </Card>
    )
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{statusLabel(p.status)}</CardTitle>
        <CardDescription>
          {p.status === 'rejected' && p.message ? `El agent dijo: ${p.message}. ` : ''}
          No se puede aceptar. Para dar de alta el proyecto, proponelo de nuevo.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <Link href={`/servers/${p.server_id}/new-project`} className="text-sm text-primary underline">
          Nuevo proyecto
        </Link>
      </CardContent>
    </Card>
  )
}

/** until dice cuánto falta para algo, en palabras. */
function until(iso: string, now: Date = new Date()): string {
  const hours = Math.round((new Date(iso).getTime() - now.getTime()) / 3_600_000)
  if (Number.isNaN(hours)) return 'pronto'
  if (hours < 1) return 'en menos de una hora'
  if (hours < 48) return `en ${hours} h`
  return `en ${Math.round(hours / 24)} días`
}
