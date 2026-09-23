/**
 * Cliente de la API del control plane.
 *
 * La sesión viaja en una cookie HttpOnly que el navegador manda sola: acá no se
 * guarda ningún token, y el código del panel nunca lo ve (ADR-010).
 */

/** ApiError es una respuesta de error de la API. */
export class ApiError extends Error {
  readonly status: number
  readonly code: string

  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }

  /** unauthorized dice si hay que volver al login. */
  get unauthorized() {
    return this.status === 401
  }
}

type ErrorBody = { error?: string; message?: string }

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let response: Response
  try {
    response = await fetch(path, {
      ...init,
      headers: { 'Content-Type': 'application/json', ...init?.headers },
      // Mismo origen: en producción el panel lo sirve el mismo binario, y en
      // desarrollo el proxy de Vite hace que lo parezca.
      credentials: 'same-origin',
    })
  } catch {
    throw new ApiError(0, 'network', 'No se pudo hablar con el control plane')
  }

  if (response.status === 204) {
    return undefined as T
  }

  const raw = await response.text()
  let body: unknown = undefined
  if (raw !== '') {
    try {
      body = JSON.parse(raw)
    } catch {
      body = undefined
    }
  }

  if (!response.ok) {
    const { error, message } = (body ?? {}) as ErrorBody
    throw new ApiError(response.status, error ?? 'unknown', message ?? 'Error del control plane')
  }
  return body as T
}

export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: 'POST', body: body === undefined ? undefined : JSON.stringify(body) }),
}

/** Session es quién está en sesión y hasta cuándo. */
export type Session = {
  username: string
  expires_at: string
}

export const auth = {
  me: () => api.get<Session>('/v1/auth/me'),
  login: (username: string, password: string) =>
    api.post<Session>('/v1/auth/login', { username, password }),
  logout: () => api.post<void>('/v1/auth/logout'),
}

/** ServerStatus es lo que devuelve la API, derivado del último latido. */
export type ServerStatus = 'online' | 'offline' | 'never_seen'

export type DiskUsage = {
  mountpoint: string
  total_bytes: number
  used_bytes: number
  available_bytes: number
  used_percent: number
}

export type LatestSample = {
  ts: string
  cpu_used_percent: number
  cpu_cores: number
  mem_total_bytes: number
  mem_used_bytes: number
  load1: number
  worst_disk: DiskUsage | null
}

export type ServerSummary = {
  id: string
  name: string
  hostname: string
  labels: Record<string, string>
  agent_version: string
  status: ServerStatus
  last_seen_at: string | null
  enrolled_at: string
  sample: LatestSample | null
}

export const servers = {
  list: () => api.get<{ servers: ServerSummary[] }>('/v1/servers').then((body) => body.servers),
}
