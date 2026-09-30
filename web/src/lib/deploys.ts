/** Textos y reglas del deploy en el panel (ADR-015, ADR-017). */

import type { DeployStatus, DeployStep, DeploySummary, StepName } from '@/lib/api'

/** active dice si un deploy todavía no terminó. */
export function active(status: DeployStatus): boolean {
  return status === 'pending' || status === 'running'
}

const statusLabels: Record<DeployStatus, string> = {
  pending: 'Pedido',
  running: 'En curso',
  succeeded: 'Desplegado',
  failed: 'Falló',
  rolled_back: 'Volvió atrás',
}

/** statusLabel es el nombre de un estado para mostrar. */
export function statusLabel(status: DeployStatus): string {
  return statusLabels[status] ?? status
}

/**
 * statusTone agrupa los estados por cómo hay que mirarlos: `rolled_back` es un
 * deploy que falló pero dejó el sitio como estaba; `failed` con
 * rollback_failed es el único que puede dejarlo a medias.
 */
export function statusTone(
  d: Pick<DeploySummary, 'status' | 'code'>,
): 'ok' | 'warn' | 'bad' | 'busy' {
  switch (d.status) {
    case 'succeeded':
      return 'ok'
    case 'rolled_back':
      return 'warn'
    case 'failed':
      return d.code === 'rollback_failed' ? 'bad' : 'warn'
    default:
      return 'busy'
  }
}

const stepLabels: Record<StepName, string> = {
  preflight: 'Chequeo previo',
  fetch: 'Traer la rama',
  snapshot: 'Anotar lo que corre',
  checkout: 'Cambiar el código',
  build: 'Compilar',
  pre: 'Paso previo',
  up: 'Levantar',
  healthcheck: 'Verificar',
  post: 'Paso posterior',
  rollback: 'Volver atrás',
}

/** stepLabel es el nombre de un paso, con su servicio si es un paso declarado. */
export function stepLabel(step: Pick<DeployStep, 'name' | 'service'>): string {
  const label = stepLabels[step.name] ?? step.name
  return step.service ? `${label} (${step.service})` : label
}

const codeTexts: Record<string, string> = {
  // Rechazos del agent.
  disabled: 'El deploy está apagado en ese servidor: lo prende una persona en el servidor.',
  busy: 'El agent ya está desplegando otra cosa; se despliega de a uno.',
  unknown_target: 'El agent no encuentra la declaración del proyecto.',
  unavailable: 'Al agent le falta algo para desplegar (git o docker).',
  agent_offline: 'El agent se desconectó antes de recibir el pedido.',
  no_answer: 'El agent nunca confirmó el pedido.',
  // Fallas del deploy.
  preflight_failed: 'El proyecto no está listo para desplegar en el servidor.',
  dirty_worktree: 'Hay archivos cambiados a mano en el servidor; el deploy no los pisa.',
  fetch_failed: 'No se pudo traer la rama.',
  commit_not_on_branch: 'Ese commit no está en la rama declarada.',
  build_failed: 'La compilación falló.',
  step_failed: 'Falló un paso declarado.',
  up_failed: 'No se pudieron levantar los contenedores.',
  unhealthy: 'No quedó sano a tiempo.',
  timeout: 'Se pasó del tiempo máximo.',
  interrupted: 'El agent se reinició en medio del deploy.',
  no_previous: 'No hay un estado anterior a donde volver.',
  missing_image: 'La imagen anterior ya no está en el servidor.',
  rollback_failed: 'Falló y tampoco pudo volver atrás: hay que mirarlo.',
}

/** codeText explica un código de error en castellano. */
export function codeText(code: string | undefined): string {
  if (!code) return ''
  return codeTexts[code] ?? code
}

/** shortCommit son los primeros 8 caracteres de un SHA. */
export function shortCommit(sha: string | undefined): string {
  return sha ? sha.slice(0, 8) : '—'
}

/** duration dice cuánto tardó algo, en palabras cortas: 45 s, 3 min 10 s. */
export function duration(from?: string, to?: string, now: Date = new Date()): string {
  if (!from) return ''
  const start = new Date(from).getTime()
  const end = to ? new Date(to).getTime() : now.getTime()
  const seconds = Math.max(0, Math.round((end - start) / 1000))
  if (Number.isNaN(seconds)) return ''
  if (seconds < 60) return `${seconds} s`
  const minutes = Math.floor(seconds / 60)
  const rest = seconds % 60
  return rest === 0 ? `${minutes} min` : `${minutes} min ${rest} s`
}

/** validCommit dice si el texto es un SHA completo, como lo exige el control plane. */
export function validCommit(sha: string): boolean {
  return /^[0-9a-f]{40}$/.test(sha)
}

/** validCode dice si el texto tiene forma de código TOTP. */
export function validCode(code: string): boolean {
  return /^\d{6}$/.test(code)
}

const requestErrors: Record<string, string> = {
  confirmation_required: 'Para confirmar hay que escribir el nombre del proyecto.',
  invalid_code: 'El código no sirve: usá uno nuevo de la app (no el que usaste para entrar).',
  deploy_in_progress: 'Ese proyecto ya tiene un deploy en curso.',
  agent_offline: 'El agent de ese servidor no está conectado.',
  unsupported: 'El deploy de proyectos de systemd todavía no existe.',
  rate_limited: 'Demasiados pedidos seguidos; esperá un minuto.',
}

/** requestError explica por qué el control plane no aceptó el pedido. */
export function requestError(code: string, message: string): string {
  return requestErrors[code] ?? message
}
