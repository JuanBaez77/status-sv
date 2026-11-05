/** Textos y reglas de las aprobaciones de los pedidos del MCP (ADR-026). */

import type { ApprovalOperation, ApprovalStatus } from '@/lib/api'

const operationLabels: Record<ApprovalOperation, string> = {
  deploy: 'Deploy',
  rollback: 'Volver atrás',
  restart: 'Reiniciar',
  bootstrap: 'Primer deploy',
}

/** operationLabel es lo que se pide, en palabras. */
export function operationLabel(op: ApprovalOperation): string {
  return operationLabels[op] ?? op
}

const statusLabels: Record<ApprovalStatus, string> = {
  pending: 'Pendiente',
  approved: 'Aprobado',
  rejected: 'Rechazado',
  expired: 'Vencido',
  cancelled: 'Cancelado',
}

/** statusLabel es el estado de un pedido, en palabras. */
export function statusLabel(status: ApprovalStatus): string {
  return statusLabels[status] ?? status
}

/**
 * commitURL arma el enlace a un commit del repo declarado, si es de GitHub.
 * El repo viene del proyecto declarado en el servidor, no del LLM.
 */
export function commitURL(repo: string | undefined, sha: string): string | undefined {
  const match = repo?.match(/^github\.com\/([A-Za-z0-9_.-]+)\/([A-Za-z0-9_.-]+?)(\.git)?$/)
  if (!match || !/^[0-9a-f]{40}$/.test(sha)) return undefined
  return `https://github.com/${match[1]}/${match[2]}/commit/${sha}`
}

/** compareURL arma el enlace a lo que cambia entre lo que corre y lo pedido. */
export function compareURL(repo: string | undefined, from: string, to: string): string | undefined {
  const base = commitURL(repo, to)
  if (!base || !/^[0-9a-f]{40}$/.test(from)) return undefined
  return base.replace(/\/commit\/[0-9a-f]{40}$/, `/compare/${from}...${to}`)
}

/** expiresIn dice cuánto le falta a un pedido para vencer. */
export function expiresIn(expiresAt: string, now: Date = new Date()): string {
  const minutes = Math.ceil((new Date(expiresAt).getTime() - now.getTime()) / 60_000)
  if (minutes <= 0) return 'venció'
  if (minutes === 1) return 'vence en 1 minuto'
  return `vence en ${minutes} minutos`
}

const errorTexts: Record<string, string> = {
  confirmation_required: 'Para aprobar hay que escribir el nombre del proyecto.',
  invalid_code: 'Hace falta un código nuevo de la app de autenticación.',
  not_pending: 'El pedido ya no está pendiente.',
  agent_offline: 'El agent no está conectado. El pedido sigue pendiente: se puede aprobar cuando vuelva.',
  deploy_in_progress: 'Ese proyecto tiene un deploy en curso. El pedido sigue pendiente.',
}

/**
 * approvalError explica un error al aprobar o rechazar. Un pedido cancelado
 * trae su motivo en el mensaje del control plane.
 */
export function approvalError(code: string, message: string): string {
  if (code === 'cancelled') return message.charAt(0).toUpperCase() + message.slice(1) + '.'
  return errorTexts[code] ?? message
}
