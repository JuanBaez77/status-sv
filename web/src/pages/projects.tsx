import { AlertTriangle } from 'lucide-react'

import { ProjectCard } from '@/components/project-card'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useProjects } from '@/hooks/use-projects'
import { statusOrder } from '@/lib/projects'
import { since } from '@/lib/format'

/**
 * ProjectsPage lista los proyectos declarados en todos los servidores, con lo
 * que tiene problemas primero, y los archivos que no se pudieron leer.
 */
export function ProjectsPage() {
  const { data, isPending, error } = useProjects()

  if (isPending) {
    return <p className="text-sm text-muted-foreground">Cargando proyectos…</p>
  }
  if (error) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>No se pudieron traer los proyectos</CardTitle>
          <CardDescription>{error.message}</CardDescription>
        </CardHeader>
      </Card>
    )
  }

  const sorted = [...data.projects].sort(
    (a, b) => statusOrder[a.status] - statusOrder[b.status] || a.name.localeCompare(b.name),
  )

  return (
    <section className="flex flex-col gap-4">
      <h1 className="text-lg font-medium">Proyectos</h1>

      {data.read_errors.map((e) => (
        <p key={e.server_id} role="alert" className="flex gap-1.5 text-sm text-destructive">
          <AlertTriangle className="mt-0.5 size-4 shrink-0" aria-hidden />
          <span>
            No se pudo leer el directorio de proyectos de {e.server_name}: {e.detail}. Lo que se ve
            {e.taken_at ? ` es de ${since(e.taken_at)}` : ' puede estar viejo'}.
          </span>
        </p>
      ))}

      {sorted.length === 0 ? (
        <Card>
          <CardHeader>
            <CardTitle>Todavía no hay proyectos declarados</CardTitle>
            <CardDescription>
              Un proyecto nuevo se propone desde la página de su servidor, con{' '}
              <span className="font-medium">Nuevo proyecto</span>, y una persona lo acepta en el
              servidor con un comando. Cada proyecto queda como un archivo en{' '}
              <code className="font-mono">/etc/status-sv/projects.d</code>.
            </CardDescription>
          </CardHeader>
        </Card>
      ) : (
        <ul className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {sorted.map((p) => (
            <li key={p.id}>
              <ProjectCard project={p} />
            </li>
          ))}
        </ul>
      )}

      {data.invalid.length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle>Archivos que no se pudieron usar</CardTitle>
            <CardDescription>
              Quedan afuera enteros hasta que se corrijan en el servidor.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <ul className="divide-y divide-border">
              {data.invalid.map((f) => (
                <li key={`${f.server_id}/${f.file}`} className="py-2 text-sm first:pt-0 last:pb-0">
                  <span className="font-medium">{f.file}</span>{' '}
                  <span className="text-muted-foreground">en {f.server_name}</span>
                  <p className="text-xs text-destructive">{f.error}</p>
                </li>
              ))}
            </ul>
          </CardContent>
        </Card>
      )}
    </section>
  )
}
