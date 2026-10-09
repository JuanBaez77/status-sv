import { describe, expect, it } from 'vitest'

import type { McpToken } from '@/lib/api'
import { envCommand, environmentsOf, expiresText, mcpCommand, validDays, validName } from '@/lib/tokens'

function token(patch: Partial<McpToken> = {}): McpToken {
  return {
    id: 't1',
    name: 'claude-code',
    prefix: 'svmcp_AbCdEf',
    status: 'active',
    scopes: ['read:staging'],
    all_projects: true,
    projects: [],
    created_by_name: 'juan',
    created_at: '2026-10-09T12:00:00Z',
    expires_at: '2026-11-08T12:00:00Z',
    revoked_at: null,
    last_used_at: null,
    ...patch,
  }
}

describe('validName', () => {
  it('acepta lo que acepta el control plane', () => {
    expect(validName('claude-code en la notebook')).toBe(true)
    expect(validName('  con espacios  ')).toBe(true)
    expect(validName('ñ'.repeat(64))).toBe(true)
  })

  it('rechaza vacíos, largos y caracteres de control', () => {
    expect(validName('   ')).toBe(false)
    expect(validName('a'.repeat(65))).toBe(false)
    expect(validName('uno\ndos')).toBe(false)
    expect(validName('tab\taquí')).toBe(false)
  })
})

describe('validDays', () => {
  it('va de 1 al máximo, sin decimales', () => {
    expect(validDays(1, 90)).toBe(true)
    expect(validDays(90, 90)).toBe(true)
    expect(validDays(0, 90)).toBe(false)
    expect(validDays(91, 90)).toBe(false)
    expect(validDays(1.5, 90)).toBe(false)
    expect(validDays(Number.NaN, 90)).toBe(false)
  })
})

describe('environmentsOf', () => {
  it('saca los entornos de los permisos', () => {
    expect([...environmentsOf(['read:staging', 'logs:staging', 'logs:production'])].sort()).toEqual([
      'production',
      'staging',
    ])
    expect(environmentsOf([]).size).toBe(0)
  })
})

describe('comandos para Claude Code', () => {
  it('el de Claude Code lleva la variable, no el token, entre comillas simples', () => {
    expect(mcpCommand('https://status.example.com')).toBe(
      "claude mcp add --transport http --scope user status-sv https://status.example.com/mcp --header 'Authorization: Bearer ${STATUSSV_MCP_TOKEN}'",
    )
  })

  it('el de la variable toma el token del portapapeles', () => {
    expect(envCommand).toBe(
      "[Environment]::SetEnvironmentVariable('STATUSSV_MCP_TOKEN', (Get-Clipboard), 'User')",
    )
    expect(envCommand).not.toMatch(/svmcp_/)
  })
})

describe('expiresText', () => {
  const now = new Date('2026-10-09T12:00:00Z')

  it('dice cuánto falta, en días redondeados desde un día y medio', () => {
    expect(expiresText(token(), now)).toBe('Vence en 30 días')
    // Un token de 7 días recién creado dice 7, no 6.
    expect(expiresText(token({ expires_at: '2026-10-16T11:59:50Z' }), now)).toBe('Vence en 7 días')
    expect(expiresText(token({ expires_at: '2026-10-11T00:00:00Z' }), now)).toBe('Vence en 2 días')
    expect(expiresText(token({ expires_at: '2026-10-10T13:00:00Z' }), now)).toBe('Vence en 25 horas')
    expect(expiresText(token({ expires_at: '2026-10-09T15:30:00Z' }), now)).toBe('Vence en 3 horas')
    expect(expiresText(token({ expires_at: '2026-10-09T12:10:00Z' }), now)).toBe('Vence en 1 hora')
  })

  it('vencido o revocado no dice nada: lo dice el estado', () => {
    expect(expiresText(token({ status: 'expired', expires_at: '2026-10-09T11:00:00Z' }), now)).toBe('')
    expect(expiresText(token({ expires_at: '2026-10-09T11:00:00Z' }), now)).toBe('')
    expect(expiresText(token({ status: 'revoked' }), now)).toBe('')
  })
})
