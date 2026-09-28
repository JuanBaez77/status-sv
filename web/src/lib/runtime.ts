/**
 * Cómo se muestra lo que corre en un servidor: el tono y el texto de cada
 * estado, y en qué orden van las unidades.
 *
 * Es cálculo puro, aparte de los componentes, porque es donde están las
 * decisiones que vale la pena probar: qué es un problema y qué no.
 */

import type {
  RuntimeContainer,
  RuntimeProcess,
  RuntimeService,
  RuntimeSource,
} from '@/lib/api'

/**
 * Tone es cómo se pinta un estado:
 *
 * - ok: funciona como tiene que funcionar.
 * - warning: está en transición o no está donde debería (un servicio
 *   habilitado que se paró y no se sabe por qué).
 * - bad: está roto y alguien tiene que mirarlo.
 * - idle: parado, y no es necesariamente un problema (un contenedor de una
 *   tarea que ya terminó, o un servicio que terminó bien).
 */
export type Tone = 'ok' | 'warning' | 'bad' | 'idle'

export type UnitStatus = { tone: Tone; label: string }

export function containerStatus(c: RuntimeContainer): UnitStatus {
  switch (c.state) {
    case 'running':
      if (c.health === 'unhealthy') return { tone: 'bad', label: 'No saludable' }
      if (c.health === 'starting') return { tone: 'warning', label: 'Arrancando' }
      return { tone: 'ok', label: c.health === 'healthy' ? 'Saludable' : 'Corriendo' }
    case 'restarting':
      return { tone: 'warning', label: 'Reiniciándose' }
    case 'paused':
      return { tone: 'warning', label: 'En pausa' }
    case 'dead':
      return { tone: 'bad', label: 'Muerto' }
    case 'exited':
      return { tone: 'idle', label: 'Detenido' }
    case 'created':
      return { tone: 'idle', label: 'Creado' }
    case 'removing':
      return { tone: 'idle', label: 'Borrándose' }
    default:
      return { tone: 'idle', label: 'Desconocido' }
  }
}

export function serviceStatus(s: RuntimeService): UnitStatus {
  switch (s.active_state) {
    case 'active':
      return { tone: 'ok', label: 'Activo' }
    case 'failed':
      return { tone: 'bad', label: 'Falló' }
    // El agent solo manda un servicio parado si está habilitado. Si terminó
    // bien, corrió y terminó solo (una tarea de arranque) o alguien lo paró a
    // propósito: no es un problema. Si no se sabe cómo terminó (un agent
    // anterior no lo dice) o terminó mal, hay que mirarlo.
    case 'inactive':
      return s.result === 'success' ? { tone: 'idle', label: 'Parado' } : { tone: 'warning', label: 'Parado' }
    case 'activating':
      return { tone: 'warning', label: 'Arrancando' }
    case 'deactivating':
      return { tone: 'warning', label: 'Deteniéndose' }
    case 'reloading':
    case 'refreshing':
      return { tone: 'warning', label: 'Recargando' }
    case 'maintenance':
      return { tone: 'warning', label: 'En mantenimiento' }
    default:
      return { tone: 'idle', label: 'Desconocido' }
  }
}

/** results traduce los motivos de systemd que más se ven. */
const results: Record<string, string> = {
  'exit-code': 'salió con error',
  signal: 'lo mató una señal',
  'core-dump': 'se colgó (core dump)',
  'oom-kill': 'se quedó sin memoria',
  timeout: 'se pasó de tiempo',
  watchdog: 'no respondió al watchdog',
  'start-limit-hit': 'demasiados reinicios seguidos',
  resources: 'no consiguió recursos para arrancar',
}

/**
 * serviceResult dice por qué terminó un servicio, si terminó mal. Un motivo
 * que no está en la lista se muestra como lo da systemd.
 */
export function serviceResult(s: RuntimeService): string | null {
  if (s.result === '' || s.result === 'success') return null
  return results[s.result] ?? s.result
}

/**
 * imageName acorta una imagen sin nombre, que Docker muestra como su hash
 * entero, a los 12 caracteres de `docker ps`.
 */
export function imageName(image: string): string {
  const hash = /^sha256:([0-9a-f]{64})$/.exec(image)
  return hash ? hash[1].slice(0, 12) : image
}

/** toneOrder pone primero lo que hay que mirar. */
const toneOrder: Record<Tone, number> = { bad: 0, warning: 1, ok: 2, idle: 3 }

/** byAttention ordena lo roto primero, después lo que está en transición, y el resto por nombre. */
export function byAttention<T>(items: T[], status: (item: T) => UnitStatus, name: (item: T) => string): T[] {
  return [...items].sort(
    (a, b) =>
      toneOrder[status(a).tone] - toneOrder[status(b).tone] || name(a).localeCompare(name(b), 'es'),
  )
}

/** Summary cuenta cuántas unidades hay en cada tono, para el encabezado de la sección. */
export type Summary = Record<Tone, number>

export function summarize<T>(items: T[], status: (item: T) => UnitStatus): Summary {
  const summary: Summary = { ok: 0, warning: 0, bad: 0, idle: 0 }
  for (const item of items) summary[status(item).tone]++
  return summary
}

/** summaryText resume una sección en una línea: "12 bien · 1 con problemas · 3 detenidos". */
export function summaryText(summary: Summary, okLabel: string, idleLabel: string): string {
  const parts: string[] = []
  if (summary.ok > 0) parts.push(`${summary.ok} ${okLabel}`)
  if (summary.bad > 0) parts.push(`${summary.bad} con problemas`)
  if (summary.warning > 0) parts.push(`${summary.warning} para revisar`)
  if (summary.idle > 0) parts.push(`${summary.idle} ${idleLabel}`)
  return parts.length === 0 ? 'Nada por ahora.' : parts.join(' · ')
}

/**
 * SourceNotice es lo que hay que avisar de una fuente: si no existe en el
 * servidor, si falló (y lo que se ve es viejo) o si quedaron unidades afuera.
 */
export type SourceNotice =
  | { kind: 'none' }
  | { kind: 'unavailable'; text: string }
  | { kind: 'error'; text: string; since: string }
  | { kind: 'omitted'; text: string }

export function sourceNotice(source: RuntimeSource | undefined, noun: string): SourceNotice {
  if (source === undefined) return { kind: 'none' }
  switch (source.status) {
    case 'unavailable':
      return { kind: 'unavailable', text: source.detail || `Este servidor no tiene ${noun}.` }
    case 'error':
      return {
        kind: 'error',
        text: source.detail || 'El agent no pudo leer esta fuente.',
        since: source.taken_at,
      }
    default:
      if (source.omitted > 0) {
        return {
          kind: 'omitted',
          text: `Hay ${source.omitted} más que no entraron en la vista.`,
        }
      }
      return { kind: 'none' }
  }
}

/** processTitle es cómo se nombra un proceso: su nombre y su PID, que es lo que se busca con top. */
export function processTitle(p: RuntimeProcess): string {
  return `${p.name} · ${p.pid}`
}
