import { describe, expect, it } from 'vitest'

import type { ProjectSummary } from '@/lib/api'
import { healthText, unitsText } from '@/lib/projects'

const base: ProjectSummary = {
  id: 'p1',
  name: 'web',
  server_id: 's1',
  server_name: 'vps',
  environment: 'production',
  runtime: 'docker',
  repo: 'github.com/x/web',
  branch: 'main',
  status: 'ok',
  units: { total: 2, healthy: 1 },
  health: null,
  seen_at: '2026-09-28T12:00:00Z',
}
const now = new Date('2026-09-28T12:01:00Z')

describe('textos de proyectos', () => {
  it('cuenta las unidades con el nombre de su runtime', () => {
    expect(unitsText(base)).toBe('1 de 2 contenedores bien')
    expect(unitsText({ ...base, runtime: 'systemd', units: { total: 0, healthy: 0 } })).toBe(
      'Sin servicios corriendo',
    )
  })

  it('describe el healthcheck', () => {
    expect(healthText(null, now)).toBe('Sin healthcheck declarado')
    const declared = { url: 'https://x/health', expect_status: 200, timeout_seconds: 10 }
    expect(healthText(declared, now)).toBe('Healthcheck todavía sin probar')
    expect(
      healthText(
        { ...declared, state: 'failing', checked_at: '2026-09-28T12:00:00Z', detail: 'timeout' },
        now,
      ),
    ).toMatch(/^Healthcheck falla: timeout/)
    expect(
      healthText(
        {
          ...declared,
          state: 'passing',
          checked_at: '2026-09-28T12:00:00Z',
          status_code: 200,
          latency_ms: 40,
        },
        now,
      ),
    ).toMatch(/^Healthcheck OK \(200, 40 ms\)/)
  })
})
