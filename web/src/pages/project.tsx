import { ArrowLeft } from 'lucide-react'
import { Link } from 'wouter'

import { DeploysCard } from '@/components/deploy-panel'
import { ProjectStatusBadge } from '@/components/project-status'
import { ContainerRow, ServiceRow } from '@/components/runtime-sections'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useProject } from '@/hooks/use-projects'
import type { ProjectDetail } from '@/lib/api'
import { containerStatus, serviceStatus, byAttention, sourceNotice } from '@/lib/runtime'
import { since } from '@/lib/format'
import { environmentLabel, healthText, unitsText } from '@/lib/projects'

/** ProjectPage muestra un proyecto: lo declarado y lo que corre de él. */
export function ProjectPage({ id }: { id: string }) {
  const { data: p, isPending, error } = useProject(id)

  if (isPending) {
    return <p className="text-sm text-muted-foreground">Cargando el proyecto…</p>
  }
  if (error) {
    return (
      <div className="flex flex-col gap-4">
        <BackLink />
        <Card>
          <CardHeader>
            <CardTitle>
              {error.status === 404 ? 'No existe ese proyecto' : 'No se pudo traer el proyecto'}
            </CardTitle>
            <CardDescription>
              {error.status === 404
                ? 'Puede que se haya dejado de declarar en su servidor.'
                : error.message}
            </CardDescription>
          </CardHeader>
        </Card>
      </div>
    )
  }

  return (
    <section className="flex flex-col gap-4">
      <BackLink />
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <h1 className="text-lg font-medium">{p.name}</h1>
          <p className="text-xs text-muted-foreground">
            <Link href={`/servers/${p.server_id}`} className="underline-offset-2 hover:underline">
              {p.server_name}
            </Link>{' '}
            · {environmentLabel(p.environment)} · {p.runtime} · leído {since(p.seen_at)}
          </p>
        </div>
        <ProjectStatusBadge status={p.status} />
      </header>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Declarado</CardTitle>
            <CardDescription>
              De <code className="font-mono">/etc/status-sv/projects.d/{p.name}.yaml</code>
            </CardDescription>
          </CardHeader>
          <CardContent>
            <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
              <Field label="Repositorio" value={p.repo} />
              <Field label="Rama" value={p.branch} />
              <Field label="Directorio" value={p.workdir} />
              {p.runtime === 'docker' ? (
                <>
                  <Field label="Compose" value={p.compose_file ?? ''} />
                  <Field label="Proyecto de Compose" value={p.compose_project ?? ''} />
                </>
              ) : (
                <Field label="Servicios" value={(p.declared_units ?? []).join(', ')} />
              )}
              <Field label="Healthcheck" value={p.health ? p.health.url : 'no declarado'} />
            </dl>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Estado</CardTitle>
            <CardDescription>{unitsText(p)}</CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-1 text-sm">
            <p>{healthText(p.health)}</p>
            {p.health && (
              <p className="text-xs text-muted-foreground">
                Espera {p.health.expect_status}, con {p.health.timeout_seconds} s de timeout. Lo
                prueba el agent desde el propio servidor.
              </p>
            )}
          </CardContent>
        </Card>
      </div>

      <DeploysCard project={p} />

      <Units project={p} />
    </section>
  )
}

function Units({ project: p }: { project: ProjectDetail }) {
  const notice = sourceNotice(p.source ?? undefined, p.runtime === 'docker' ? 'Docker' : 'systemd')
  const title = p.runtime === 'docker' ? 'Contenedores' : 'Servicios'
  const containers = byAttention(p.containers, containerStatus, (c) => c.name)
  const services = byAttention(p.services, serviceStatus, (s) => s.unit)

  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
        <CardDescription>
          {p.runtime === 'docker'
            ? `Los contenedores con la etiqueta de Compose ${p.compose_project}. Los que terminaron (por ejemplo, migraciones) se muestran pero no cuentan para el estado.`
            : 'Los servicios declarados en units.'}
        </CardDescription>
      </CardHeader>
      <CardContent>
        {notice.kind !== 'none' && (
          <p className="mb-2 text-xs text-muted-foreground">{notice.text}</p>
        )}
        {p.missing_units.length > 0 && (
          <p role="alert" className="mb-2 text-xs text-destructive">
            El agent no ve {p.missing_units.length === 1 ? 'el servicio' : 'los servicios'}{' '}
            {p.missing_units.join(', ')}: puede no existir o estar excluido.
          </p>
        )}
        {containers.length === 0 && services.length === 0 ? (
          <p className="text-xs text-muted-foreground">No corre nada de este proyecto.</p>
        ) : (
          <ul className="divide-y divide-border">
            {containers.map((c) => (
              <ContainerRow key={c.id} serverID={p.server_id} container={c} />
            ))}
            {services.map((s) => (
              <ServiceRow key={s.unit} serverID={p.server_id} service={s} />
            ))}
          </ul>
        )}
      </CardContent>
    </Card>
  )
}

function Field({ label, value }: { label: string; value: string }) {
  return (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0 break-all font-mono text-xs leading-5">{value}</dd>
    </>
  )
}

function BackLink() {
  return (
    <Link
      href="/projects"
      className="inline-flex w-fit items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
    >
      <ArrowLeft className="size-3" aria-hidden />
      Proyectos
    </Link>
  )
}
