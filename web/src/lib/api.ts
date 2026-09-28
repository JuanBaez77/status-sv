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

/** Resolution es el detalle con el que vienen las series. */
export type Resolution = 'raw' | '1m' | '1h'

/**
 * SamplePoint es un punto de la serie de una muestra.
 *
 * En resolución `1m` los campos `*_max` son el pico del minuto y el resto es el
 * promedio; en `raw` los dos son el mismo valor, el de la muestra. En `1h` es
 * igual que en `1m`, y además llegan los `*_min` de CPU y memoria: el promedio
 * por minuto más bajo de la hora.
 */
export type SamplePoint = {
  ts: string
  cpu_used_percent: number
  cpu_used_max_percent: number
  cpu_used_min_percent?: number
  cpu_iowait_percent: number
  cpu_iowait_max_percent: number
  cpu_steal_percent: number
  cpu_cores: number
  mem_total_bytes: number
  mem_used_bytes: number
  mem_used_max_bytes: number
  mem_used_min_bytes?: number
  swap_total_bytes: number
  swap_used_bytes: number
  swap_used_max_bytes: number
  load1: number
  load5: number
  load15: number
}

export type DiskPoint = {
  /** mountpoint solo viene en los puntos en vivo; en las series lo lleva DiskSeries. */
  mountpoint?: string
  ts: string
  total_bytes: number
  used_bytes: number
  available_bytes: number
}

export type DiskSeries = {
  mountpoint: string
  points: DiskPoint[]
}

/** NetworkPoint son tasas, no contadores: el panel grafica bytes por segundo. */
export type NetworkPoint = {
  /** interface solo viene en los puntos en vivo; en las series lo lleva NetworkSeries. */
  interface?: string
  ts: string
  rx_bytes_per_sec: number
  rx_max_bytes_per_sec: number
  tx_bytes_per_sec: number
  tx_max_bytes_per_sec: number
}

export type NetworkSeries = {
  interface: string
  points: NetworkPoint[]
}

export type ServerMetrics = {
  server_id: string
  resolution: Resolution
  from: string
  to: string
  /** truncated dice que el rango tenía más puntos de los que entraron. */
  truncated: boolean
  samples: SamplePoint[]
  disks: DiskSeries[]
  networks: NetworkSeries[]
}

/** RuntimeUsage falta (null) cuando no hay medición, que no es lo mismo que cero. */
export type RuntimeUsage = {
  cpu_percent: number
  memory_bytes: number
}

/**
 * RuntimeSource dice si el agent pudo leer una fuente. Con `error`, lo que se
 * muestra de esa fuente es lo último que se supo.
 */
export type RuntimeSource = {
  status: 'ok' | 'unavailable' | 'error'
  detail?: string
  /** omitted son las unidades que no entraron en la foto por los topes. */
  omitted: number
  taken_at: string
}

export type RuntimeContainer = {
  id: string
  name: string
  image: string
  state: 'created' | 'running' | 'paused' | 'restarting' | 'removing' | 'exited' | 'dead' | 'unknown'
  /** health vacío es un contenedor sin healthcheck. */
  health: '' | 'starting' | 'healthy' | 'unhealthy'
  started_at: string | null
  compose_project: string
  compose_service: string
  usage: RuntimeUsage | null
  seen_at: string
}

export type RuntimeService = {
  unit: string
  load_state: string
  active_state:
    | 'active'
    | 'reloading'
    | 'inactive'
    | 'failed'
    | 'activating'
    | 'deactivating'
    | 'maintenance'
    | 'refreshing'
    | 'unknown'
  sub_state: string
  /**
   * result es cómo terminó la última vez, con los nombres de systemd
   * ("success", "exit-code", "oom-kill"...). Vacío si está corriendo o si el
   * agent es anterior y no lo manda.
   */
  result: string
  main_pid: number
  active_since: string | null
  usage: RuntimeUsage | null
  seen_at: string
}

export type RuntimeProcess = {
  pid: number
  name: string
  /** command ya viene con los secretos enmascarados desde el agent. */
  command: string
  user: string
  started_at: string | null
  usage: RuntimeUsage | null
  seen_at: string
}

/** ServerRuntime es lo que corre en un servidor: la misma forma por la API y por el canal en vivo. */
export type ServerRuntime = {
  server_id: string
  /** taken_at es null si el agent todavía no mandó ninguna foto. */
  taken_at: string | null
  sources: Partial<Record<'docker' | 'systemd' | 'processes', RuntimeSource>>
  containers: RuntimeContainer[]
  services: RuntimeService[]
  processes: RuntimeProcess[]
}

export type LogKind = 'container' | 'service'

/** LogLine es un renglón de log, con los secretos ya enmascarados por el agent. */
export type LogLine = {
  ts?: string
  stream?: 'stdout' | 'stderr'
  text: string
}

/**
 * ServerLogs es la respuesta del agent a un pedido de logs. Un pedido que el
 * agent rechazó o que falló viene con status distinto de ok y el motivo, no
 * como error de HTTP.
 */
export type ServerLogs = {
  status: 'ok' | 'rejected' | 'failed'
  code?: string
  message?: string
  lines: LogLine[]
  /** truncated dice que se dejaron afuera los renglones más viejos para que entraran. */
  truncated: boolean
}

export const servers = {
  list: () => api.get<{ servers: ServerSummary[] }>('/v1/servers').then((body) => body.servers),

  metrics: (id: string, from: Date, to: Date) => {
    const query = new URLSearchParams({ from: from.toISOString(), to: to.toISOString() })
    return api.get<ServerMetrics>(`/v1/servers/${encodeURIComponent(id)}/metrics?${query}`)
  },

  logs: (id: string, kind: LogKind, target: string, lines: number) =>
    api.post<ServerLogs>(`/v1/servers/${encodeURIComponent(id)}/logs`, { kind, target, lines }),

  runtime: (id: string) => {
    return api.get<ServerRuntime>(`/v1/servers/${encodeURIComponent(id)}/runtime`)
  },
}
