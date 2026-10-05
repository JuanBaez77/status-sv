/** Textos y orden de los proyectos, compartidos por la lista y el detalle. */

import type { ProjectHealth, ProjectStatus, ProjectSummary } from '@/lib/api'
import { since } from '@/lib/format'

/** statusOrder pone primero lo que necesita atención. */
export const statusOrder: Record<ProjectStatus, number> = {
  down: 0,
  degraded: 1,
  unknown: 2,
  ok: 3,
}

export function environmentLabel(env: ProjectSummary['environment']): string {
  return env === 'production' ? 'producción' : 'staging'
}

export function unitsText(p: ProjectSummary): string {
  const noun = p.runtime === 'docker' ? 'contenedores' : 'servicios'
  if (p.units.total === 0) return `Sin ${noun} corriendo`
  return `${p.units.healthy} de ${p.units.total} ${noun} bien`
}

/**
 * missingUnitsText avisa de los servicios declarados que el agent no ve. El
 * agent lista un servicio parado solo si está habilitado, así que uno que
 * espera su primer deploy (ADR-021) también cae acá.
 */
export function missingUnitsText(units: string[]): string {
  const noun = units.length === 1 ? 'el servicio' : 'los servicios'
  return `El agent no ve ${noun} ${units.join(', ')}: puede no existir, estar excluido o estar parado y sin habilitar, como uno que espera su primer deploy.`
}

export function healthText(h: ProjectHealth | null, now: Date = new Date()): string {
  if (h === null) return 'Sin healthcheck declarado'
  if (!h.state || !h.checked_at) return 'Healthcheck todavía sin probar'
  if (h.state === 'passing') {
    return `Healthcheck OK (${h.status_code}, ${h.latency_ms} ms) ${since(h.checked_at, now)}`
  }
  const why = h.status_code ? `respondió ${h.status_code}` : (h.detail ?? 'sin respuesta')
  return `Healthcheck falla: ${why}, ${since(h.checked_at, now)}`
}
