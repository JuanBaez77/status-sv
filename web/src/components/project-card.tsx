import { Link } from 'wouter'

import { ProjectStatusBadge } from '@/components/project-status'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import type { ProjectSummary } from '@/lib/api'
import { environmentLabel, healthText, unitsText } from '@/lib/projects'

/** ProjectCard es un proyecto en una lista: la de proyectos y la de un servidor. */
export function ProjectCard({ project: p }: { project: ProjectSummary }) {
  return (
    <Link href={`/projects/${p.id}`} className="block h-full">
      <Card className="h-full transition-colors hover:border-primary/50">
        <CardHeader>
          <div className="flex items-start justify-between gap-2">
            <CardTitle className="truncate">{p.name}</CardTitle>
            <ProjectStatusBadge status={p.status} className="shrink-0" />
          </div>
          <CardDescription>
            {p.server_name} · {environmentLabel(p.environment)} · {p.runtime}
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-1 text-xs text-muted-foreground">
          <span>{unitsText(p)}</span>
          <span>{healthText(p.health)}</span>
        </CardContent>
      </Card>
    </Link>
  )
}
