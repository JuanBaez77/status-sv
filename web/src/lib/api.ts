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

/**
 * SessionStage es el paso del login (ADR-013). Solo `full` ve el panel: con la
 * contraseña se llega a `totp_pending` (falta el código de la app) o a
 * `totp_enroll` (falta dar de alta el segundo factor).
 */
export type SessionStage = 'full' | 'totp_pending' | 'totp_enroll'

/** Session es quién está en sesión, hasta cuándo y en qué paso del login. */
export type Session = {
  username: string
  expires_at: string
  stage: SessionStage
  /** recovery_codes_left llega solo con la sesión completa. */
  recovery_codes_left?: number
}

/** TotpEnrollment es lo que se carga en la app de autenticación. */
export type TotpEnrollment = {
  secret: string
  otpauth_uri: string
  /** qr_png es la imagen del QR como data URL. */
  qr_png: string
}

/** TotpConfirmation trae los códigos de recuperación, que se ven una sola vez. */
export type TotpConfirmation = Session & { recovery_codes: string[] }

export const auth = {
  me: () => api.get<Session>('/v1/auth/me'),
  login: (username: string, password: string) =>
    api.post<Session>('/v1/auth/login', { username, password }),
  logout: () => api.post<void>('/v1/auth/logout'),
  totpEnroll: () => api.post<TotpEnrollment>('/v1/auth/totp/enroll'),
  totpConfirm: (code: string) => api.post<TotpConfirmation>('/v1/auth/totp/confirm', { code }),
  totpVerify: (input: { code: string } | { recovery_code: string }) =>
    api.post<Session>('/v1/auth/totp/verify', input),
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

/**
 * ProjectStatus es el estado de un proyecto, que el control plane deriva de lo
 * que el agent ve correr (ADR-012):
 *
 * - `ok`: corre todo lo que tiene que correr y el healthcheck pasa.
 * - `degraded`: corre algo pero no todo, o el healthcheck falla.
 * - `down`: no corre nada.
 * - `unknown`: el agent no pudo leer Docker o systemd.
 */
export type ProjectStatus = 'ok' | 'degraded' | 'down' | 'unknown'

/** ProjectHealth es el healthcheck declarado y su último resultado, si hay. */
export type ProjectHealth = {
  url: string
  expect_status: number
  timeout_seconds: number
  state?: 'passing' | 'failing'
  checked_at?: string
  status_code?: number
  latency_ms?: number
  detail?: string
}

export type ProjectSummary = {
  id: string
  name: string
  server_id: string
  server_name: string
  environment: 'production' | 'staging'
  runtime: 'docker' | 'systemd'
  repo: string
  branch: string
  status: ProjectStatus
  /** units cuenta lo que tiene que correr y cuánto de eso está bien. */
  units: { total: number; healthy: number }
  health: ProjectHealth | null
  seen_at: string
}

export type ProjectDetail = ProjectSummary & {
  workdir: string
  compose_file?: string
  compose_project?: string
  declared_units?: string[]
  /** missing_units son los servicios declarados que el agent no ve. */
  missing_units: string[]
  containers: RuntimeContainer[]
  services: RuntimeService[]
  /** source dice si el agent pudo leer Docker o systemd; null si nunca mandó nada. */
  source: RuntimeSource | null
}

/** InvalidProjectFile es un project.yaml que no se pudo usar: nunca trae el contenido. */
export type InvalidProjectFile = {
  server_id: string
  server_name: string
  file: string
  error: string
  seen_at: string
}

/** ProjectReadError es un servidor cuya última lectura de proyectos falló. */
export type ProjectReadError = {
  server_id: string
  server_name: string
  detail: string
  taken_at: string | null
}

export type ProjectList = {
  projects: ProjectSummary[]
  invalid: InvalidProjectFile[]
  read_errors: ProjectReadError[]
}

export const projects = {
  list: () => api.get<ProjectList>('/v1/projects'),
  get: (id: string) => api.get<ProjectDetail>(`/v1/projects/${encodeURIComponent(id)}`),
}

/**
 * DeployStatus es cómo va un deploy (ADR-015). `pending` es que el control
 * plane lo anotó y el agent todavía no mandó su primera foto.
 */
export type DeployStatus = 'pending' | 'running' | 'succeeded' | 'failed' | 'rolled_back'

/** DeployKind: `bootstrap` es el primer deploy de un proyecto (ADR-021). */
export type DeployKind = 'deploy' | 'rollback' | 'bootstrap'

export type StepName =
  | 'preflight'
  | 'fetch'
  | 'snapshot'
  | 'checkout'
  | 'build'
  | 'pre'
  | 'up'
  | 'restart'
  | 'healthcheck'
  | 'post'
  | 'rollback'

/** DeployStep es un paso del deploy con su resultado y los últimos renglones de su salida. */
export type DeployStep = {
  name: StepName
  /** service va en los pasos declarados (pre y post). */
  service?: string
  status: 'running' | 'ok' | 'failed' | 'skipped'
  started_at?: string
  finished_at?: string
  detail?: string
  output?: string[]
}

export type DeploySummary = {
  id: string
  kind: DeployKind
  /** requested_commit falta si se pidió la punta de la rama. */
  requested_commit?: string
  commit?: string
  previous_commit?: string
  status: DeployStatus
  code?: string
  message?: string
  requested_by: string
  requested_at: string
  started_at?: string
  finished_at?: string
}

export type DeployDetail = DeploySummary & {
  project_id: string | null
  project_name: string
  server_id: string
  server_name: string
  environment: 'production' | 'staging'
  steps: DeployStep[]
}

/** DeployRequest es el pedido: confirm es el nombre del proyecto, escrito a mano. */
export type DeployRequest = {
  confirm: string
  commit?: string
  /**
   * totp_code es un código nuevo de la app; hace falta en producción (ADR-017)
   * y siempre en un primer deploy (ADR-021).
   */
  totp_code?: string
}

export const deploys = {
  history: (projectID: string) =>
    api.get<{ deploys: DeploySummary[] }>(`/v1/projects/${encodeURIComponent(projectID)}/deploys`),
  get: (id: string) => api.get<DeployDetail>(`/v1/deploys/${encodeURIComponent(id)}`),
  start: (projectID: string, req: DeployRequest) =>
    api.post<DeployDetail>(`/v1/projects/${encodeURIComponent(projectID)}/deploys`, req),
  rollback: (projectID: string, req: DeployRequest) =>
    api.post<DeployDetail>(`/v1/projects/${encodeURIComponent(projectID)}/rollback`, req),
  bootstrap: (projectID: string, req: DeployRequest) =>
    api.post<DeployDetail>(`/v1/projects/${encodeURIComponent(projectID)}/bootstrap`, req),
}

/** AuditEntry es un renglón de la auditoría: quién hizo qué, sobre qué y cómo salió (ADR-020). */
export type AuditEntry = {
  id: string
  at: string
  actor: string
  actor_type: 'user' | 'llm' | 'agent' | 'system'
  action: string
  target_type: string
  target_id: string
  result: string
  ip?: string
  detail?: Record<string, unknown>
}

export type AuditPage = {
  entries: AuditEntry[]
  /** next_cursor falta cuando no hay más renglones. */
  next_cursor?: string
}

export const audit = {
  /** list trae una página; query son los parámetros ya armados (lib/audit). */
  list: (query: string) => api.get<AuditPage>(query === '' ? '/v1/audit' : `/v1/audit?${query}`),
}
