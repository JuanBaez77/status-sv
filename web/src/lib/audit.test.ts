import { describe, expect, it } from 'vitest'

import type { AuditEntry } from '@/lib/api'
import {
  actionLabel,
  auditLink,
  auditQuery,
  detailLines,
  filtersFromSearch,
  noFilters,
  resultLabel,
  resultTone,
  searchFromFilters,
  targetText,
} from '@/lib/audit'

function entry(overrides: Partial<AuditEntry> = {}): AuditEntry {
  return {
    id: '0199',
    at: '2026-10-02T06:10:00Z',
    actor: 'juan',
    actor_type: 'user',
    action: 'deploy.request',
    target_type: 'project',
    target_id: 'id-del-proyecto',
    result: 'accepted',
    ...overrides,
  }
}

describe('filtros en la dirección', () => {
  it('se leen y se vuelven a escribir sin perder nada', () => {
    const filters = filtersFromSearch('action=auth.login&result=failed&from=2026-10-01&actor=claude-code&otro=x')
    expect(filters).toEqual({
      ...noFilters,
      action: 'auth.login',
      result: 'failed',
      from: '2026-10-01',
      actor: 'claude-code',
    })
    expect(searchFromFilters(filters)).toBe(
      '/audit?actor=claude-code&action=auth.login&result=failed&from=2026-10-01',
    )
  })

  it('sin filtros, la dirección queda limpia', () => {
    expect(searchFromFilters(noFilters)).toBe('/audit')
  })

  it('el enlace de un proyecto trae su filtro', () => {
    expect(auditLink('project', 'abc')).toBe('/audit?target_type=project&target_id=abc')
  })
})

describe('auditQuery', () => {
  it('manda solo los filtros puestos, y el cursor', () => {
    const query = new URLSearchParams(
      auditQuery({ ...noFilters, action: 'auth.login', target_id: 'abc' }, '123_id'),
    )
    expect([...query.keys()].sort()).toEqual(['action', 'before', 'target_id'])
    expect(query.get('before')).toBe('123_id')
  })

  it('sin filtros ni cursor no manda nada', () => {
    expect(auditQuery(noFilters, null)).toBe('')
  })

  it('el día "hasta" es inclusive: manda el comienzo del día siguiente', () => {
    const query = new URLSearchParams(
      auditQuery({ ...noFilters, from: '2026-10-01', to: '2026-10-01' }, null),
    )
    const from = new Date(query.get('from') as string)
    const to = new Date(query.get('to') as string)
    expect(to.getTime() - from.getTime()).toBe(24 * 60 * 60 * 1000)
    // El día empieza a medianoche en la hora de quien mira.
    expect(from.getTime()).toBe(new Date(2026, 9, 1).getTime())
  })

  it('un día mal escrito no filtra', () => {
    expect(auditQuery({ ...noFilters, from: 'ayer', to: '2026-13-45x' }, null)).toBe('')
  })
})

describe('textos', () => {
  it('una acción o un resultado que el panel no conoce se muestran tal cual', () => {
    expect(actionLabel('auth.login')).toBe('Ingresó la contraseña')
    expect(actionLabel('algo.nuevo')).toBe('algo.nuevo')
    expect(resultLabel('rolled_back')).toBe('Volvió atrás')
    expect(resultLabel('otro')).toBe('otro')
  })

  it('un deploy que volvió atrás o un login frenado se marcan como algo para mirar', () => {
    expect(resultTone('ok')).toBe('good')
    expect(resultTone('succeeded')).toBe('good')
    expect(resultTone('rolled_back')).toBe('bad')
    expect(resultTone('blocked')).toBe('bad')
    expect(resultTone('busy')).toBe('neutral')
  })

  it('el objetivo se muestra por nombre si el detalle lo trae', () => {
    expect(targetText(entry({ detail: { proyecto: 'gcg-alertas', servidor: 'sv-gcg' } }))).toBe(
      'gcg-alertas',
    )
    expect(targetText(entry({ target_type: 'server', detail: { servidor: 'sv-gcg' } }))).toBe(
      'sv-gcg',
    )
    expect(targetText(entry())).toBe('id-del-proyecto')
    expect(targetText(entry({ detail: { proyecto: 7 } }))).toBe('id-del-proyecto')
    // Una llamada del MCP sin objetivo, como list_servers.
    expect(targetText(entry({ target_type: 'mcp', target_id: '-' }))).toBe('')
  })

  it('el detalle se aplana en orden, con lo que no es texto como JSON', () => {
    expect(detailLines({ renglones: 200, commit: 'abc', con_codigo: true })).toEqual([
      ['commit', 'abc'],
      ['con_codigo', 'true'],
      ['renglones', '200'],
    ])
    expect(detailLines(undefined)).toEqual([])
  })
})
