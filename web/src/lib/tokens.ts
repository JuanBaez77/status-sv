import type { McpToken, TokenScope, TokenStatus } from '@/lib/api'

/** maxNameLength es el largo máximo del nombre de un token, en caracteres. */
export const maxNameLength = 64

/** scopeGroups ordena los permisos como los muestra el formulario: por entorno. */
export const scopeGroups: { environment: string; label: string; scopes: { scope: TokenScope; label: string }[] }[] = [
  {
    environment: 'staging',
    label: 'Staging',
    scopes: [
      { scope: 'read:staging', label: 'Leer' },
      { scope: 'logs:staging', label: 'Logs' },
      { scope: 'deploy:staging', label: 'Deploy' },
      { scope: 'rollback:staging', label: 'Rollback' },
      { scope: 'restart:staging', label: 'Reiniciar' },
    ],
  },
  {
    environment: 'production',
    label: 'Producción',
    scopes: [
      { scope: 'read:production', label: 'Leer' },
      { scope: 'logs:production', label: 'Logs' },
      { scope: 'deploy:production', label: 'Deploy' },
      { scope: 'rollback:production', label: 'Rollback' },
      { scope: 'restart:production', label: 'Reiniciar' },
    ],
  },
]

const scopeLabels: Record<TokenScope, string> = {
  'read:staging': 'Leer staging',
  'read:production': 'Leer producción',
  'logs:staging': 'Logs de staging',
  'logs:production': 'Logs de producción',
  'deploy:staging': 'Deploy en staging',
  'deploy:production': 'Deploy en producción',
  'rollback:staging': 'Rollback en staging',
  'rollback:production': 'Rollback en producción',
  'restart:staging': 'Reiniciar en staging',
  'restart:production': 'Reiniciar en producción',
}

/** writes dice si un permiso deja pedir una escritura: deploy, rollback o reinicio. */
export function writes(scope: TokenScope): boolean {
  return /^(deploy|rollback|restart):/.test(scope)
}

/** scopeLabel dice un permiso en palabras. */
export function scopeLabel(scope: TokenScope): string {
  return scopeLabels[scope] ?? scope
}

const statusLabels: Record<TokenStatus, string> = {
  active: 'Activo',
  expired: 'Vencido',
  revoked: 'Revocado',
}

/** statusLabel dice el estado de un token en palabras. */
export function statusLabel(status: TokenStatus): string {
  return statusLabels[status] ?? status
}

/** validName dice si el nombre se puede usar: lo que el control plane acepta. */
export function validName(name: string): boolean {
  const trimmed = name.trim()
  const chars = [...trimmed]
  return trimmed !== '' && chars.length <= maxNameLength && !chars.some(isControl)
}

/** isControl dice si un carácter es de control, que el control plane rechaza en un nombre. */
function isControl(char: string): boolean {
  const code = char.codePointAt(0) ?? 0
  return code < 0x20 || (code >= 0x7f && code < 0xa0)
}

/** validDays dice si el vencimiento está entre 1 y el máximo. */
export function validDays(days: number, max: number): boolean {
  return Number.isInteger(days) && days >= 1 && days <= max
}

/** environmentsOf devuelve los entornos que un conjunto de permisos alcanza. */
export function environmentsOf(scopes: TokenScope[]): Set<string> {
  return new Set(scopes.map((scope) => scope.split(':')[1]))
}

/** tokenVariable es la variable de entorno donde vive el token en la máquina de quien lo usa. */
export const tokenVariable = 'STATUSSV_MCP_TOKEN'

/**
 * envCommand guarda el token en una variable de entorno del usuario de
 * Windows, leyéndolo del portapapeles: así no queda escrito en el historial
 * de PowerShell.
 */
export const envCommand = `[Environment]::SetEnvironmentVariable('${tokenVariable}', (Get-Clipboard), 'User')`

/**
 * mcpCommand conecta Claude Code al MCP. El token no va en el comando: va
 * `${STATUSSV_MCP_TOKEN}` literal, entre comillas simples, que Claude Code
 * expande al conectarse. Con --scope user queda en la configuración de la
 * persona, no en un repo.
 */
export function mcpCommand(origin: string): string {
  return `claude mcp add --transport http --scope user status-sv ${origin}/mcp --header 'Authorization: Bearer \${${tokenVariable}}'`
}

/**
 * expiresText dice cuándo vence un token, o nada si ya no sirve: el estado ya
 * dice si venció o lo revocaron. Desde un día y medio, en días redondeados (un
 * token de 7 días recién creado dice 7, no 6); antes, en horas.
 */
export function expiresText(token: McpToken, now: Date = new Date()): string {
  if (token.status !== 'active') return ''
  const hours = (new Date(token.expires_at).getTime() - now.getTime()) / 3_600_000
  if (hours <= 0) return ''
  if (hours >= 36) return `Vence en ${Math.round(hours / 24)} días`
  const whole = Math.max(1, Math.floor(hours))
  return whole === 1 ? 'Vence en 1 hora' : `Vence en ${whole} horas`
}

const requestErrors: Record<string, string> = {
  invalid_code: 'El código no sirve: hace falta uno nuevo de la app de autenticación.',
  rate_limited: 'Demasiados pedidos seguidos: esperá un minuto.',
}

/** requestError dice en palabras por qué el control plane no creó o no revocó el token. */
export function requestError(code: string, message: string): string {
  return requestErrors[code] ?? message
}
