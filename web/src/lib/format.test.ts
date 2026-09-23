import { describe, expect, it } from 'vitest'

import { bytes, percent, perSecond, ratio, since } from '@/lib/format'

describe('bytes', () => {
  it('elige la unidad binaria que corresponde', () => {
    expect(bytes(512)).toBe('512 B')
    expect(bytes(1024)).toBe('1 KiB')
    expect(bytes(1536)).toBe('1,5 KiB')
    expect(bytes(8 * 1024 ** 3)).toBe('8 GiB')
  })

  it('no inventa nada con valores imposibles', () => {
    expect(bytes(-1)).toBe('—')
    expect(bytes(Number.NaN)).toBe('—')
  })
})

describe('porcentajes', () => {
  it('formatea con coma decimal', () => {
    expect(percent(42.75)).toBe('42,8 %')
    expect(ratio(1, 4)).toBe('25 %')
  })

  it('un total en cero no divide por cero', () => {
    expect(ratio(1, 0)).toBe('—')
  })
})

describe('perSecond', () => {
  it('agrega la unidad de tiempo', () => {
    expect(perSecond(2048)).toBe('2 KiB/s')
  })
})

describe('since', () => {
  const now = new Date('2026-09-22T12:00:00Z')

  it('dice hace cuánto, en palabras', () => {
    expect(since('2026-09-22T11:59:48Z', now)).toBe('hace 12 s')
    expect(since('2026-09-22T11:57:00Z', now)).toBe('hace 3 min')
    expect(since('2026-09-22T09:00:00Z', now)).toBe('hace 3 h')
    expect(since('2026-09-20T12:00:00Z', now)).toBe('hace 2 d')
  })

  it('un servidor que nunca se vio lo dice', () => {
    expect(since(null, now)).toBe('nunca')
  })

  it('un reloj adelantado no muestra tiempos negativos', () => {
    expect(since('2026-09-22T12:00:05Z', now)).toBe('recién')
  })
})
