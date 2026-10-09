import { ChevronDown, ChevronRight } from 'lucide-react'
import { useState, type ReactNode } from 'react'
import { useSearchParams } from 'wouter'

import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { useAudit } from '@/hooks/use-audit'
import type { AuditEntry } from '@/lib/api'
import {
  actionLabel,
  actionLabels,
  actorTypeLabels,
  detailLines,
  filtersFromSearch,
  noFilters,
  resultLabel,
  results,
  resultTone,
  targetText,
  when,
  type AuditFilters,
  type Tone,
} from '@/lib/audit'
import { since } from '@/lib/format'
import { cn } from '@/lib/utils'

const selectClass =
  'h-9 w-full rounded-md border border-input bg-background px-2 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring'

/**
 * AuditPage muestra la auditoría: quién hizo qué, sobre qué y cómo salió, del
 * renglón más nuevo al más viejo (ADR-020). Los filtros van en la dirección
 * de la página, así un enlace desde un proyecto los trae puestos.
 */
export function AuditPage() {
  const [params, setParams] = useSearchParams()
  const filters = filtersFromSearch(params.toString())
  const query = useAudit(filters)

  function change(patch: Partial<AuditFilters>) {
    const next = { ...filters, ...patch }
    setParams(
      (current) => {
        const updated = new URLSearchParams(current)
        for (const [name, value] of Object.entries(next)) {
          if (value === '') updated.delete(name)
          else updated.set(name, value)
        }
        return updated
      },
      { replace: true },
    )
  }

  const entries = (query.data ?? []).flatMap((page) => page.entries)
  const filtered = Object.values(filters).some((value) => value !== '')

  return (
    <section className="flex flex-col gap-4">
      <header className="flex flex-col gap-1">
        <h1 className="text-lg font-medium">Auditoría</h1>
        <p className="text-xs text-muted-foreground">
          Lo que se hizo desde el panel, el MCP, los agents y los subcomandos del servidor. No se puede
          editar ni borrar.
        </p>
      </header>

      <Card>
        <CardContent className="flex flex-col gap-3 pt-6">
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
            <Field label="Acción" htmlFor="audit-action">
              <select
                id="audit-action"
                className={selectClass}
                value={filters.action}
                onChange={(e) => change({ action: e.target.value })}
              >
                <option value="">Todas</option>
                {Object.entries(actionLabels).map(([value, label]) => (
                  <option key={value} value={value}>
                    {label}
                  </option>
                ))}
              </select>
            </Field>
            <Field label="Quién" htmlFor="audit-actor-type">
              <select
                id="audit-actor-type"
                className={selectClass}
                value={filters.actor_type}
                onChange={(e) => change({ actor_type: e.target.value })}
              >
                <option value="">Cualquiera</option>
                {Object.entries(actorTypeLabels).map(([value, label]) => (
                  <option key={value} value={value}>
                    {label}
                  </option>
                ))}
              </select>
            </Field>
            <Field label="Resultado" htmlFor="audit-result">
              <select
                id="audit-result"
                className={selectClass}
                value={filters.result}
                onChange={(e) => change({ result: e.target.value })}
              >
                <option value="">Cualquiera</option>
                {results.map((value) => (
                  <option key={value} value={value}>
                    {resultLabel(value)}
                  </option>
                ))}
              </select>
            </Field>
            <Field label="Desde" htmlFor="audit-from">
              <Input
                id="audit-from"
                type="date"
                value={filters.from}
                max={filters.to || undefined}
                onChange={(e) => change({ from: e.target.value })}
              />
            </Field>
            <Field label="Hasta" htmlFor="audit-to">
              <Input
                id="audit-to"
                type="date"
                value={filters.to}
                min={filters.from || undefined}
                onChange={(e) => change({ to: e.target.value })}
              />
            </Field>
          </div>

          {(filters.target_id !== '' || filters.actor !== '' || filtered) && (
            <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
              {filters.actor !== '' && (
                <span>
                  Solo lo que hizo <strong className="font-medium text-foreground">{filters.actor}</strong>.
                </span>
              )}
              {filters.target_id !== '' && (
                <span>
                  Solo lo {targetTypeLabel(filters.target_type)}{' '}
                  <code className="font-mono">{filters.target_id}</code>.
                </span>
              )}
              <Button variant="ghost" size="sm" onClick={() => change(noFilters)}>
                Quitar los filtros
              </Button>
            </div>
          )}
        </CardContent>
      </Card>

      {query.isPending ? (
        <p className="text-sm text-muted-foreground">Cargando la auditoría…</p>
      ) : query.error && entries.length === 0 ? (
        <Card>
          <CardHeader>
            <CardTitle>No se pudo traer la auditoría</CardTitle>
            <CardDescription>{query.error.message}</CardDescription>
          </CardHeader>
        </Card>
      ) : entries.length === 0 ? (
        <Card>
          <CardHeader>
            <CardTitle>
              {filtered ? 'Nada coincide con esos filtros' : 'Todavía no hay nada anotado'}
            </CardTitle>
          </CardHeader>
        </Card>
      ) : (
        <Card>
          <CardContent className="pt-6">
            <ul className="divide-y divide-border" aria-label="Renglones de auditoría">
              {entries.map((entry) => (
                <Row key={entry.id} entry={entry} />
              ))}
            </ul>

            <div className="mt-4 flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
              {query.hasNextPage ? (
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => void query.fetchNextPage()}
                  disabled={query.isFetchingNextPage}
                >
                  {query.isFetchingNextPage ? 'Cargando…' : 'Cargar más'}
                </Button>
              ) : (
                <span>No hay más renglones.</span>
              )}
              {query.error && (
                <span role="alert" className="text-destructive">
                  No se pudo traer la página siguiente: {query.error.message}
                </span>
              )}
            </div>
          </CardContent>
        </Card>
      )}
    </section>
  )
}

function Field({
  label,
  htmlFor,
  children,
}: {
  label: string
  htmlFor: string
  children: ReactNode
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={htmlFor}>{label}</Label>
      {children}
    </div>
  )
}

function targetTypeLabel(type: string): string {
  switch (type) {
    case 'project':
      return 'del proyecto'
    case 'server':
      return 'del servidor'
    case 'deploy':
      return 'del deploy'
    case 'api_token':
      return 'del token'
    case 'user':
      return 'del usuario'
    default:
      return 'del objetivo'
  }
}

const toneClass: Record<Tone, string> = {
  good: 'bg-status-online',
  bad: 'bg-status-offline',
  neutral: 'bg-status-unknown',
}

/** Row es un renglón; el detalle se abre a pedido. */
function Row({ entry }: { entry: AuditEntry }) {
  const [open, setOpen] = useState(false)
  const lines = detailLines(entry.detail)
  const target = targetText(entry)

  return (
    <li className="py-2.5 first:pt-0 last:pb-0">
      <button
        type="button"
        className="flex w-full items-start gap-2 text-left"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
      >
        {open ? (
          <ChevronDown className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden />
        ) : (
          <ChevronRight className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden />
        )}
        <span className="flex min-w-0 flex-1 flex-col gap-0.5">
          <span className="text-sm">
            <span className="font-medium">{entry.actor}</span>{' '}
            <span className="text-muted-foreground">
              ({actorTypeLabels[entry.actor_type] ?? entry.actor_type})
            </span>{' '}
            · {actionLabel(entry.action)}
            {target !== '' && target !== entry.actor && (
              <>
                {' '}
                · <span className="break-all">{target}</span>
              </>
            )}
          </span>
          <span className="text-xs text-muted-foreground">
            <time dateTime={entry.at} title={since(entry.at)}>
              {when(entry.at)}
            </time>
            {entry.ip ? ` · ${entry.ip}` : ''}
          </span>
        </span>
        <span className="inline-flex shrink-0 items-center gap-1.5 text-xs">
          <span
            className={cn('size-2 rounded-full', toneClass[resultTone(entry.result)])}
            aria-hidden
          />
          {resultLabel(entry.result)}
        </span>
      </button>

      {open && (
        <dl className="mt-2 ml-6 grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-xs">
          <dt className="text-muted-foreground">acción</dt>
          <dd className="font-mono">{entry.action}</dd>
          <dt className="text-muted-foreground">objetivo</dt>
          <dd className="font-mono break-all">
            {entry.target_type} {entry.target_id}
          </dd>
          {lines.map(([key, value]) => (
            <div key={key} className="contents">
              <dt className="text-muted-foreground">{key}</dt>
              <dd className="font-mono break-all">{value}</dd>
            </div>
          ))}
        </dl>
      )}
    </li>
  )
}
