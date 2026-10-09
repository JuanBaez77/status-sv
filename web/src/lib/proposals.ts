/** El alta de proyectos en el panel (ADR-022): el formulario y los textos de las propuestas. */

import type { ProjectDetail, ProposalRequest, ProposalStatus, ProposalStep } from '@/lib/api'

/**
 * ProposalForm es el formulario tal cual se escribe. Los comandos van como
 * texto, un paso por renglón; toRequest los parte en argumentos.
 */
export type ProposalForm = {
  name: string
  environment: 'production' | 'staging'
  repo: string
  branch: string
  runtime: 'docker' | 'systemd'
  composeFile: string
  command: string
  requiredEnv: string
  siteDomain: string
  sitePort: string
  healthcheckURL: string
  build: string
  pre: string
  post: string
}

export const emptyForm: ProposalForm = {
  name: '',
  environment: 'staging',
  repo: '',
  branch: 'main',
  runtime: 'systemd',
  composeFile: 'docker-compose.yml',
  command: '',
  requiredEnv: '',
  siteDomain: '',
  sitePort: '',
  healthcheckURL: '',
  build: '',
  pre: '',
  post: '',
}

/** FormError es un campo del formulario que no se puede mandar así. */
export class FormError extends Error {
  readonly field: keyof ProposalForm

  constructor(field: keyof ProposalForm, message: string) {
    super(message)
    this.name = 'FormError'
    this.field = field
  }
}

/**
 * splitArgs parte un comando en argumentos: por espacios, y entre comillas
 * dobles un argumento puede tenerlos. No hay shell en el medio: lo que se
 * escribe es lo que se ejecuta, argumento por argumento.
 */
export function splitArgs(line: string): string[] | null {
  const args: string[] = []
  let current = ''
  let quoted = false
  let started = false
  for (const char of line) {
    if (char === '"') {
      quoted = !quoted
      started = true
    } else if (!quoted && /\s/.test(char)) {
      if (started) args.push(current)
      current = ''
      started = false
    } else {
      current += char
      started = true
    }
  }
  if (quoted) return null
  if (started) args.push(current)
  return args
}

/** serviceLine es un paso de Docker: el servicio de Compose, dos puntos y el comando. */
const serviceLine = /^([A-Za-z0-9_.-]+):\s+(.+)$/

/**
 * parseSteps lee los pasos de un campo, uno por renglón. Con Docker, cada
 * paso corre en un servicio de Compose y se escribe `servicio: comando`.
 */
export function parseSteps(
  field: 'build' | 'pre' | 'post',
  text: string,
  runtime: ProposalForm['runtime'],
): ProposalStep[] {
  const steps: ProposalStep[] = []
  text.split('\n').forEach((raw, i) => {
    const line = raw.trim()
    if (line === '') return
    let service: string | undefined
    let rest = line
    if (runtime === 'docker') {
      const match = serviceLine.exec(line)
      if (!match) {
        throw new FormError(field, `Renglón ${i + 1}: con Docker es "servicio: comando".`)
      }
      service = match[1]
      rest = match[2]
    }
    const command = splitArgs(rest)
    if (command === null || command.length === 0) {
      throw new FormError(field, `Renglón ${i + 1}: falta cerrar unas comillas.`)
    }
    steps.push(service ? { service, command } : { command })
  })
  return steps
}

const envName = /^[A-Z_][A-Z0-9_]*$/

/**
 * parseEnvNames lee los nombres de las variables requeridas. Si alguien
 * escribe un valor (`CLAVE=algo`), no se manda nada: el valor se carga en el
 * servidor, y el error no lo repite.
 */
export function parseEnvNames(text: string): string[] {
  const names = text.split(/[\s,]+/).filter((name) => name !== '')
  for (const name of names) {
    if (name.includes('=')) {
      throw new FormError(
        'requiredEnv',
        'Solo los nombres: los valores se cargan en el servidor al aceptar, y no pasan por el panel.',
      )
    }
    if (!envName.test(name)) {
      throw new FormError(
        'requiredEnv',
        'Cada nombre va en mayúsculas, con números y _, sin empezar con número.',
      )
    }
  }
  if (new Set(names).size !== names.length) {
    throw new FormError('requiredEnv', 'Hay un nombre repetido.')
  }
  return names
}

/**
 * toRequest arma el pedido desde el formulario. Revisa lo que se puede
 * revisar acá; las reglas de verdad las aplica el control plane, con el
 * mismo parser que el agent, y su error dice el campo.
 */
export function toRequest(form: ProposalForm): ProposalRequest {
  const req: ProposalRequest = {
    name: form.name.trim(),
    environment: form.environment,
    repo: form.repo.trim(),
    branch: form.branch.trim(),
    runtime: form.runtime,
  }
  if (req.name === '') throw new FormError('name', 'Falta el nombre.')
  if (req.repo === '') throw new FormError('repo', 'Falta el repositorio.')
  if (req.branch === '') throw new FormError('branch', 'Falta la rama.')

  if (form.runtime === 'docker') {
    req.compose_file = form.composeFile.trim()
  } else {
    const command = splitArgs(form.command.trim())
    if (command === null) throw new FormError('command', 'Falta cerrar unas comillas.')
    if (command.length === 0) {
      throw new FormError('command', 'Falta el comando del servicio.')
    }
    req.command = command
  }

  const env = parseEnvNames(form.requiredEnv)
  if (env.length > 0) req.required_env = env

  const domain = form.siteDomain.trim()
  const port = form.sitePort.trim()
  if (domain !== '' || port !== '') {
    if (domain === '') throw new FormError('siteDomain', 'Falta el dominio del sitio.')
    if (!/^\d+$/.test(port)) {
      throw new FormError('sitePort', 'El puerto es un número, entre 1024 y 65535.')
    }
    req.site = { domain, port: Number(port) }
  }

  const health = form.healthcheckURL.trim()
  if (health !== '') req.healthcheck = { url: health }

  if (form.runtime === 'systemd') {
    const build = parseSteps('build', form.build, form.runtime)
    if (build.length > 0) req.build = build
  }
  const pre = parseSteps('pre', form.pre, form.runtime)
  if (pre.length > 0) req.pre = pre
  const post = parseSteps('post', form.post, form.runtime)
  if (post.length > 0) req.post = post
  return req
}

const statusLabels: Record<ProposalStatus, string> = {
  pending: 'Todavía no llegó al servidor',
  delivered: 'Esperando que la acepten en el servidor',
  accepted: 'Aceptada',
  rejected: 'Rechazada por el agent',
  withdrawn: 'Retirada',
  expired: 'Venció sin aceptarse',
  superseded: 'Reemplazada por otra',
}

/** statusLabel dice en qué anda una propuesta. */
export function statusLabel(status: ProposalStatus): string {
  return statusLabels[status] ?? status
}

/** isOpen dice si la propuesta todavía se puede aceptar o retirar. */
export function isOpen(status: ProposalStatus): boolean {
  return status === 'pending' || status === 'delivered'
}

/** statusTone agrupa los estados por cómo hay que mirarlos. */
export function statusTone(status: ProposalStatus): 'ok' | 'busy' | 'bad' | 'neutral' {
  switch (status) {
    case 'accepted':
      return 'ok'
    case 'pending':
    case 'delivered':
      return 'busy'
    case 'rejected':
      return 'bad'
    default:
      return 'neutral'
  }
}

/** envCommand es cómo se cargan o cambian las variables en el servidor. */
export function envCommand(name: string): string {
  return `sudo statussv-agent project env ${name}`
}

/**
 * envText dice si están cargadas las variables requeridas de un proyecto. El
 * panel nunca ve sus valores, solo los nombres de las que faltan.
 */
export function envText(env: NonNullable<ProjectDetail['env']>): string {
  if (env.unknown) {
    return 'No se pudo mirar si están cargadas: con systemd las mira el ayudante root, que puede no estar instalado.'
  }
  if (env.missing.length === 0) {
    return env.required === 1
      ? 'Está cargada la variable requerida.'
      : `Están cargadas las ${env.required} variables requeridas.`
  }
  return `Faltan ${env.missing.length} de ${env.required}: ${env.missing.join(', ')}. Sin ellas, el deploy se frena antes de empezar.`
}
