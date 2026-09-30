import { describe, expect, it } from 'vitest'

import {
  active,
  codeText,
  duration,
  requestError,
  shortCommit,
  statusLabel,
  statusTone,
  stepLabel,
  validCode,
  validCommit,
} from '@/lib/deploys'

describe('deploys', () => {
  it('sabe qué está activo', () => {
    expect(active('pending')).toBe(true)
    expect(active('running')).toBe(true)
    expect(active('succeeded')).toBe(false)
    expect(active('rolled_back')).toBe(false)
  })

  it('distingue lo que volvió atrás de lo que quedó a medias', () => {
    expect(statusTone({ status: 'succeeded' })).toBe('ok')
    expect(statusTone({ status: 'rolled_back', code: 'unhealthy' })).toBe('warn')
    expect(statusTone({ status: 'failed', code: 'dirty_worktree' })).toBe('warn')
    expect(statusTone({ status: 'failed', code: 'rollback_failed' })).toBe('bad')
    expect(statusTone({ status: 'running' })).toBe('busy')
  })

  it('nombra los pasos, con el servicio de los declarados', () => {
    expect(stepLabel({ name: 'build' })).toBe('Compilar')
    expect(stepLabel({ name: 'pre', service: 'backend' })).toBe('Paso previo (backend)')
  })

  it('explica los códigos y deja pasar los que no conoce', () => {
    expect(codeText('disabled')).toMatch(/apagado/)
    expect(codeText('rollback_failed')).toMatch(/hay que mirarlo/)
    expect(codeText('algo_nuevo')).toBe('algo_nuevo')
    expect(codeText(undefined)).toBe('')
    expect(statusLabel('rolled_back')).toBe('Volvió atrás')
  })

  it('formatea commits y duraciones', () => {
    expect(shortCommit('89abcdef0123456789abcdef0123456789abcdef')).toBe('89abcdef')
    expect(shortCommit(undefined)).toBe('—')
    expect(duration('2026-09-30T12:00:00Z', '2026-09-30T12:00:45Z')).toBe('45 s')
    expect(duration('2026-09-30T12:00:00Z', '2026-09-30T12:03:10Z')).toBe('3 min 10 s')
    expect(duration('2026-09-30T12:00:00Z', '2026-09-30T12:02:00Z')).toBe('2 min')
    expect(duration(undefined)).toBe('')
    expect(duration('2026-09-30T12:00:00Z', undefined, new Date('2026-09-30T12:00:30Z'))).toBe(
      '30 s',
    )
  })

  it('valida commits y códigos como el control plane', () => {
    expect(validCommit('89abcdef0123456789abcdef0123456789abcdef')).toBe(true)
    expect(validCommit('89abcdef')).toBe(false)
    expect(validCommit('main')).toBe(false)
    expect(validCommit('89ABCDEF0123456789ABCDEF0123456789ABCDEF')).toBe(false)
    expect(validCode('123456')).toBe(true)
    expect(validCode('12345')).toBe(false)
    expect(validCode('12a456')).toBe(false)
  })

  it('explica los rechazos del pedido', () => {
    expect(requestError('invalid_code', 'x')).toMatch(/código nuevo|uno nuevo/)
    expect(requestError('otro', 'mensaje del servidor')).toBe('mensaje del servidor')
  })
})
