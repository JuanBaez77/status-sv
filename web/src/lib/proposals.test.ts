import { describe, expect, it } from 'vitest'

import {
  emptyForm,
  envText,
  FormError,
  isOpen,
  parseEnvNames,
  parseSteps,
  splitArgs,
  statusLabel,
  toRequest,
  type ProposalForm,
} from '@/lib/proposals'

function form(patch: Partial<ProposalForm>): ProposalForm {
  return {
    ...emptyForm,
    name: 'svdemo',
    repo: 'github.com/JuanBaez77/status-sv-demo',
    command: '.venv/bin/gunicorn app:app --bind 127.0.0.1:18097',
    ...patch,
  }
}

function errorOf(fn: () => unknown): FormError {
  try {
    fn()
  } catch (err) {
    if (err instanceof FormError) return err
    throw err
  }
  throw new Error('no falló')
}

describe('splitArgs', () => {
  it('parte por espacios y respeta las comillas dobles', () => {
    expect(splitArgs('  bin/app   --name "con espacios" x')).toEqual(['bin/app', '--name', 'con espacios', 'x'])
    expect(splitArgs('a "b"c d')).toEqual(['a', 'bc', 'd'])
    expect(splitArgs('')).toEqual([])
  })

  it('no interpreta nada de un shell', () => {
    expect(splitArgs('echo $HOME; rm -rf / && `id`')).toEqual(['echo', '$HOME;', 'rm', '-rf', '/', '&&', '`id`'])
  })

  it('avisa si quedan comillas abiertas', () => {
    expect(splitArgs('bin/app "sin cerrar')).toBeNull()
  })
})

describe('parseSteps', () => {
  it('con systemd, un paso por renglón', () => {
    expect(parseSteps('build', '/usr/bin/python3 -m venv .venv\n\n  .venv/bin/pip install -r requirements.txt ', 'systemd')).toEqual([
      { command: ['/usr/bin/python3', '-m', 'venv', '.venv'] },
      { command: ['.venv/bin/pip', 'install', '-r', 'requirements.txt'] },
    ])
  })

  it('con Docker, cada paso dice su servicio', () => {
    expect(parseSteps('pre', 'web: python manage.py migrate', 'docker')).toEqual([
      { service: 'web', command: ['python', 'manage.py', 'migrate'] },
    ])
    // Los dos puntos de un argumento no se confunden con el servicio.
    expect(parseSteps('pre', 'web: bin/run app:app', 'docker')[0].command).toEqual(['bin/run', 'app:app'])
    const err = errorOf(() => parseSteps('pre', 'python manage.py migrate', 'docker'))
    expect(err.field).toBe('pre')
    expect(err.message).toContain('Renglón 1')
  })
})

describe('parseEnvNames', () => {
  it('lee nombres separados por coma o espacio', () => {
    expect(parseEnvNames('SECRET_KEY, DATABASE_URL\nAPI_TOKEN')).toEqual(['SECRET_KEY', 'DATABASE_URL', 'API_TOKEN'])
    expect(parseEnvNames('   ')).toEqual([])
  })

  it('rechaza un valor sin repetirlo', () => {
    const err = errorOf(() => parseEnvNames('SECRET_KEY=s3cr3t0'))
    expect(err.field).toBe('requiredEnv')
    expect(err.message).not.toContain('s3cr3t0')
    expect(err.message).toContain('Solo los nombres')
  })

  it('rechaza nombres inválidos y repetidos', () => {
    expect(errorOf(() => parseEnvNames('secret_key')).field).toBe('requiredEnv')
    expect(errorOf(() => parseEnvNames('1KEY')).field).toBe('requiredEnv')
    expect(errorOf(() => parseEnvNames('KEY, KEY')).message).toContain('repetido')
  })
})

describe('toRequest', () => {
  it('arma un proyecto systemd con su sitio y sus pasos', () => {
    const req = toRequest(
      form({
        requiredEnv: 'SECRET_KEY',
        siteDomain: 'svdemo.extendsystem.com',
        sitePort: '18097',
        healthcheckURL: 'http://127.0.0.1:18097/health',
        build: '/usr/bin/python3 -m venv .venv',
        composeFile: 'no-va.yml',
      }),
    )
    expect(req).toEqual({
      name: 'svdemo',
      environment: 'staging',
      repo: 'github.com/JuanBaez77/status-sv-demo',
      branch: 'main',
      runtime: 'systemd',
      command: ['.venv/bin/gunicorn', 'app:app', '--bind', '127.0.0.1:18097'],
      required_env: ['SECRET_KEY'],
      site: { domain: 'svdemo.extendsystem.com', port: 18097 },
      healthcheck: { url: 'http://127.0.0.1:18097/health' },
      build: [{ command: ['/usr/bin/python3', '-m', 'venv', '.venv'] }],
    })
  })

  it('arma un proyecto Docker sin lo de systemd', () => {
    const req = toRequest(form({ runtime: 'docker', build: 'no/va', pre: 'web: bin/migrate' }))
    expect(req.compose_file).toBe('docker-compose.yml')
    expect(req.command).toBeUndefined()
    expect(req.build).toBeUndefined()
    expect(req.pre).toEqual([{ service: 'web', command: ['bin/migrate'] }])
  })

  it('dice qué campo falta', () => {
    expect(errorOf(() => toRequest(form({ name: ' ' }))).field).toBe('name')
    expect(errorOf(() => toRequest(form({ repo: '' }))).field).toBe('repo')
    expect(errorOf(() => toRequest(form({ command: '' }))).field).toBe('command')
    expect(errorOf(() => toRequest(form({ command: 'bin/app "x' }))).field).toBe('command')
    expect(errorOf(() => toRequest(form({ sitePort: '18097' }))).field).toBe('siteDomain')
    expect(errorOf(() => toRequest(form({ siteDomain: 'a.com', sitePort: 'ochenta' }))).field).toBe('sitePort')
  })
})

describe('estado de una propuesta', () => {
  it('dice si sigue abierta', () => {
    expect(isOpen('pending')).toBe(true)
    expect(isOpen('delivered')).toBe(true)
    expect(isOpen('accepted')).toBe(false)
    expect(isOpen('superseded')).toBe(false)
    expect(statusLabel('delivered')).toBe('Esperando que la acepten en el servidor')
  })
})

describe('envText', () => {
  it('dice qué variables faltan, por nombre', () => {
    expect(envText({ required: 3, missing: ['SECRET_KEY', 'DATABASE_URL'], unknown: false })).toBe(
      'Faltan 2 de 3: SECRET_KEY, DATABASE_URL. Sin ellas, el deploy se frena antes de empezar.',
    )
    expect(envText({ required: 1, missing: [], unknown: false })).toBe('Está cargada la variable requerida.')
    expect(envText({ required: 2, missing: [], unknown: false })).toBe('Están cargadas las 2 variables requeridas.')
    expect(envText({ required: 2, missing: [], unknown: true })).toContain('No se pudo mirar')
  })
})
