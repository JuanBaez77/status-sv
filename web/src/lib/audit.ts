import type { AuditEntry } from '@/lib/api'

/**
 * AuditFilters son los filtros de la página de auditoría (ADR-020). Viven en
 * la dirección de la página, así un enlace puede traerlos puestos. `from` y
 * `to` son días (AAAA-MM-DD), los dos inclusive.
 */
export type AuditFilters = {
  actor_type: string
  action: string
  result: string
  target_type: string
  target_id: string
  from: string
  to: string
}

export const noFilters: AuditFilters = {
  actor_type: '',
  action: '',
  result: '',
  target_type: '',
  target_id: '',
  from: '',
  to: '',
}

const filterNames = Object.keys(noFilters) as (keyof AuditFilters)[]

/** filtersFromSearch lee los filtros de la dirección de la página. */
export function filtersFromSearch(search: string): AuditFilters {
  const params = new URLSearchParams(search)
  const filters = { ...noFilters }
  for (const name of filterNames) {
    filters[name] = params.get(name) ?? ''
  }
  return filters
}

/** searchFromFilters arma la dirección de la página con los filtros puestos. */
export function searchFromFilters(filters: AuditFilters): string {
  const params = new URLSearchParams()
  for (const name of filterNames) {
    if (filters[name] !== '') params.set(name, filters[name])
  }
  const search = params.toString()
  return search === '' ? '/audit' : `/audit?${search}`
}

/** auditLink es la página de auditoría filtrada por un objetivo. */
export function auditLink(targetType: string, targetID: string): string {
  return searchFromFilters({ ...noFilters, target_type: targetType, target_id: targetID })
}

const day = /^\d{4}-\d{2}-\d{2}$/

/**
 * dayStart devuelve el instante en que empieza un día, en la hora de quien
 * mira, corrido `offset` días. Un día mal escrito no filtra.
 */
function dayStart(value: string, offset: number): string | null {
  if (!day.test(value)) return null
  const [y, m, d] = value.split('-').map(Number)
  const date = new Date(y, m - 1, d + offset)
  return Number.isNaN(date.getTime()) ? null : date.toISOString()
}

/**
 * auditQuery arma los parámetros de GET /v1/audit. `to` es inclusive en la
 * página y exclusive en la API: se manda el comienzo del día siguiente.
 */
export function auditQuery(filters: AuditFilters, before: string | null): string {
  const params = new URLSearchParams()
  for (const name of ['actor_type', 'action', 'result', 'target_type', 'target_id'] as const) {
    if (filters[name] !== '') params.set(name, filters[name])
  }
  const from = dayStart(filters.from, 0)
  if (from) params.set('from', from)
  const to = dayStart(filters.to, 1)
  if (to) params.set('to', to)
  if (before) params.set('before', before)
  return params.toString()
}

/** actionLabels dice en palabras cada acción que se audita. */
export const actionLabels: Record<string, string> = {
  'deploy.request': 'Pidió un deploy',
  'rollback.request': 'Pidió volver atrás',
  'bootstrap.request': 'Pidió un primer deploy',
  'deploy.finish': 'Terminó un deploy',
  'logs.request': 'Pidió logs',
  'project.propose': 'Propuso un proyecto nuevo',
  'project.withdraw': 'Retiró una propuesta',
  'project.accept': 'Se aceptó un proyecto en el servidor',
  'auth.login': 'Ingresó la contraseña',
  'auth.totp_verify': 'Ingresó el código del segundo factor',
  'auth.totp_enroll': 'Dio de alta el segundo factor',
  'auth.recovery_code': 'Entró con un código de recuperación',
  'auth.logout': 'Cerró la sesión',
  'agent.enroll': 'Enroló un agent',
  'user.create': 'Creó un usuario',
  'user.password': 'Cambió una contraseña',
  'user.totp_reset': 'Borró un segundo factor',
  'enrollment_token.create': 'Generó un token de enrolamiento',
}

/** actionLabel devuelve la acción en palabras, o su nombre si es una que el panel no conoce. */
export function actionLabel(action: string): string {
  return actionLabels[action] ?? action
}

export const actorTypeLabels: Record<string, string> = {
  user: 'Persona',
  llm: 'LLM',
  agent: 'Agent',
  system: 'Sistema',
}

const resultLabels: Record<string, string> = {
  ok: 'Bien',
  failed: 'Falló',
  blocked: 'Frenado por el límite',
  rejected: 'Rechazado',
  accepted: 'Aceptado',
  error: 'Error',
  succeeded: 'Desplegado',
  rolled_back: 'Volvió atrás',
}

/** resultLabel devuelve el resultado en palabras, o tal cual si no se conoce. */
export function resultLabel(result: string): string {
  return resultLabels[result] ?? result
}

export type Tone = 'good' | 'bad' | 'neutral'

/** resultTone agrupa los resultados por cómo hay que mirarlos. */
export function resultTone(result: string): Tone {
  switch (result) {
    case 'ok':
    case 'accepted':
    case 'succeeded':
      return 'good'
    case 'failed':
    case 'blocked':
    case 'rejected':
    case 'error':
    case 'rolled_back':
      return 'bad'
    default:
      return 'neutral'
  }
}

/** results son los resultados por los que se puede filtrar. */
export const results = Object.keys(resultLabels)

/**
 * targetText dice sobre qué fue la acción. Si el detalle trae el nombre (el
 * proyecto, el servidor), se muestra ese y no el identificador.
 */
export function targetText(entry: AuditEntry): string {
  const detail = entry.detail ?? {}
  const name =
    entry.target_type === 'project'
      ? detail.proyecto
      : entry.target_type === 'server'
        ? detail.servidor
        : undefined
  if (typeof name === 'string' && name !== '') return name
  return entry.target_id
}

/** detailLines aplana el detalle de un renglón para mostrarlo: clave y valor. */
export function detailLines(detail: Record<string, unknown> | undefined): [string, string][] {
  return Object.entries(detail ?? {})
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([key, value]) => [key, typeof value === 'string' ? value : JSON.stringify(value)])
}

const moment = new Intl.DateTimeFormat('es-AR', { dateStyle: 'short', timeStyle: 'medium' })

/** when formatea el instante de un renglón, en la hora de quien mira. */
export function when(iso: string): string {
  const date = new Date(iso)
  return Number.isNaN(date.getTime()) ? '—' : moment.format(date)
}
