import { describe, expect, it } from 'vitest'

import type { DiskPoint, NetworkPoint, SamplePoint, ServerMetrics } from '@/lib/api'
import type { LiveSample } from '@/lib/panel-socket'
import {
  cpuChart,
  currentReading,
  diskChart,
  loadChart,
  memoryChart,
  mergeLive,
  networkChart,
} from '@/lib/series'

const serverID = '01924f9c-0000-7000-8000-0000000000aa'
const gib = 1024 ** 3

function sampleAt(ts: string, overrides: Partial<SamplePoint> = {}): SamplePoint {
  return {
    ts,
    cpu_used_percent: 10,
    cpu_used_max_percent: 40,
    cpu_iowait_percent: 1,
    cpu_iowait_max_percent: 3,
    cpu_steal_percent: 0,
    cpu_cores: 4,
    mem_total_bytes: 8 * gib,
    mem_used_bytes: 2 * gib,
    mem_used_max_bytes: 3 * gib,
    swap_total_bytes: 0,
    swap_used_bytes: 0,
    swap_used_max_bytes: 0,
    load1: 0.5,
    load5: 0.4,
    load15: 0.3,
    ...overrides,
  }
}

function metrics(overrides: Partial<ServerMetrics> = {}): ServerMetrics {
  const samples = overrides.samples ?? [
    sampleAt('2026-09-23T12:00:00Z'),
    sampleAt('2026-09-23T12:00:10Z'),
  ]
  return {
    server_id: serverID,
    resolution: 'raw',
    // Salvo que la prueba diga otra cosa, el rango es el de las muestras: los
    // extremos del rango entran en el eje de todos los gráficos.
    from: samples[0]?.ts ?? '2026-09-23T12:00:00Z',
    to: samples.at(-1)?.ts ?? '2026-09-23T12:00:20Z',
    truncated: false,
    samples,
    disks: [],
    networks: [],
    ...overrides,
  }
}

function disk(ts: string, used: number, mountpoint?: string): DiskPoint {
  return {
    mountpoint,
    ts,
    total_bytes: 100 * gib,
    used_bytes: used,
    available_bytes: 100 * gib - used,
  }
}

function network(ts: string, rx: number, name?: string): NetworkPoint {
  return {
    interface: name,
    ts,
    rx_bytes_per_sec: rx,
    rx_max_bytes_per_sec: rx,
    tx_bytes_per_sec: rx / 2,
    tx_max_bytes_per_sec: rx / 2,
  }
}

function live(ts: string, overrides: Partial<LiveSample> = {}): LiveSample {
  return {
    server_id: serverID,
    sample: sampleAt(ts, { cpu_used_percent: 80 }),
    disks: [],
    networks: [],
    ...overrides,
  }
}

describe('mergeLive', () => {
  it('pega al final las muestras que llegaron por el canal', () => {
    const merged = mergeLive(metrics(), [live('2026-09-23T12:00:20Z')])

    expect(merged.samples).toHaveLength(3)
    expect(merged.samples.at(-1)?.ts).toBe('2026-09-23T12:00:20Z')
    expect(merged.to).toBe('2026-09-23T12:00:20Z')
  })

  it('descarta lo que ya está en la serie', () => {
    const merged = mergeLive(metrics(), [
      live('2026-09-23T12:00:00Z'),
      live('2026-09-23T12:00:10Z'),
      live('2026-09-23T12:00:20Z'),
    ])

    expect(merged.samples.map((sample) => sample.ts)).toEqual([
      '2026-09-23T12:00:00Z',
      '2026-09-23T12:00:10Z',
      '2026-09-23T12:00:20Z',
    ])
  })

  it('no toca el agregado por minuto', () => {
    const minute = metrics({ resolution: '1m' })

    // Una muestra suelta no es el promedio de un minuto: mezclarlas en la misma
    // línea diría algo que no es.
    expect(mergeLive(minute, [live('2026-09-23T12:00:20Z')])).toBe(minute)
  })

  it('suma un disco o una interfaz que no estaban en el rango', () => {
    const merged = mergeLive(metrics(), [
      live('2026-09-23T12:00:20Z', {
        disks: [disk('2026-09-23T12:00:20Z', 50 * gib, '/srv')],
        networks: [network('2026-09-23T12:00:20Z', 1000, 'eth0')],
      }),
    ])

    expect(merged.disks).toEqual([
      { mountpoint: '/srv', points: [disk('2026-09-23T12:00:20Z', 50 * gib, '/srv')] },
    ])
    expect(merged.networks[0].interface).toBe('eth0')
  })

  it('no modifica lo que le llega', () => {
    const original = metrics({
      disks: [{ mountpoint: '/', points: [disk('2026-09-23T12:00:10Z', 10 * gib)] }],
    })

    mergeLive(original, [
      live('2026-09-23T12:00:20Z', { disks: [disk('2026-09-23T12:00:20Z', 11 * gib, '/')] }),
    ])

    expect(original.samples).toHaveLength(2)
    expect(original.disks[0].points).toHaveLength(1)
  })
})

describe('gráficos', () => {
  it('corta la línea cuando el agent estuvo caído', () => {
    const data = cpuChart(
      metrics({
        samples: [
          sampleAt('2026-09-23T12:00:00Z'),
          sampleAt('2026-09-23T12:00:10Z'),
          sampleAt('2026-09-23T12:00:20Z'),
          sampleAt('2026-09-23T12:05:00Z'),
        ],
      }),
    )

    // Entre las 12:00:20 y las 12:05 no midió nadie: va un hueco, no una recta.
    expect(data.x).toHaveLength(5)
    expect(data.lines[0].values[3]).toBeNull()
    expect(data.lines[0].values[4]).toBe(10)
  })

  it('no corta por una demora chica', () => {
    const data = cpuChart(
      metrics({
        samples: [
          sampleAt('2026-09-23T12:00:00Z'),
          sampleAt('2026-09-23T12:00:10Z'),
          sampleAt('2026-09-23T12:00:35Z'),
        ],
      }),
    )

    expect(data.lines[0].values).not.toContain(null)
  })

  it('dibuja el pico solo cuando el punto es un promedio', () => {
    expect(cpuChart(metrics()).lines.map((line) => line.label)).toEqual(['Uso', 'Espera de E/S'])
    expect(cpuChart(metrics({ resolution: '1m' })).lines.map((line) => line.label)).toEqual([
      'Uso',
      'Pico',
      'Espera de E/S',
    ])
  })

  it('por hora dibuja además el mínimo, de CPU y de memoria', () => {
    const hourly = metrics({
      resolution: '1h',
      samples: [
        sampleAt('2026-09-23T12:00:00Z', { cpu_used_min_percent: 4, mem_used_min_bytes: gib }),
      ],
    })

    const cpu = cpuChart(hourly)
    expect(cpu.lines.map((line) => line.label)).toEqual(['Uso', 'Pico', 'Mínimo', 'Espera de E/S'])
    expect(cpu.lines[2].values).toContain(4)

    const memory = memoryChart(hourly)
    expect(memory.lines.map((line) => line.label)).toEqual(['En uso', 'Pico', 'Mínimo'])
    expect(memory.lines[2].values).toContain(gib)
  })

  it('muestra el swap solo si el servidor tiene', () => {
    expect(memoryChart(metrics()).lines.map((line) => line.label)).toEqual(['En uso'])

    const withSwap = metrics({
      samples: [sampleAt('2026-09-23T12:00:00Z', { swap_total_bytes: gib, swap_used_bytes: 100 })],
    })
    expect(memoryChart(withSwap).lines.map((line) => line.label)).toContain('Swap')
  })

  it('pasa el disco a porcentaje del total', () => {
    const data = diskChart(
      metrics({
        disks: [{ mountpoint: '/', points: [disk('2026-09-23T12:00:00Z', 93 * gib)] }],
      }),
    )

    expect(data.lines[0].label).toBe('/')
    expect(data.lines[0].values[0]).toBe(93)
  })

  it('la carga son las tres ventanas del kernel', () => {
    expect(loadChart(metrics()).lines.map((line) => line.label)).toEqual([
      '1 min',
      '5 min',
      '15 min',
    ])
  })

  it('la red separa entrada de salida', () => {
    const serie = { interface: 'eth0', points: [network('2026-09-23T12:00:00Z', 2048)] }
    const data = networkChart(metrics({ networks: [serie] }), serie)

    expect(data.lines.map((line) => line.label)).toEqual(['Entrada', 'Salida'])
    expect(data.lines[0].values[0]).toBe(2048)
    expect(data.lines[1].values[0]).toBe(1024)
  })

  it('todos los gráficos abarcan la misma ventana', () => {
    // La interfaz se agregó hace un rato: aunque tenga un solo punto, su
    // gráfico tiene que empezar y terminar donde los demás, o comparar un pico
    // de CPU con uno de red engañaría.
    const eth0 = { interface: 'eth0', points: [network('2026-09-23T12:00:10Z', 2048)] }
    const datos = metrics({
      from: '2026-09-23T12:00:00Z',
      to: '2026-09-23T12:00:20Z',
      networks: [eth0],
    })

    const ventana = (x: number[]) => [x[0], x.at(-1)]
    expect(ventana(networkChart(datos, eth0).x)).toEqual(ventana(cpuChart(datos).x))
    expect(ventana(diskChart(datos).x)).toEqual(ventana(cpuChart(datos).x))
  })

  it('cada línea de disco tiene su propio color', () => {
    const data = diskChart(
      metrics({
        disks: [
          { mountpoint: '/', points: [disk('2026-09-23T12:00:00Z', gib)] },
          { mountpoint: '/boot', points: [disk('2026-09-23T12:00:00Z', gib)] },
        ],
      }),
    )

    expect(new Set(data.lines.map((line) => line.color)).size).toBe(2)
  })
})

describe('currentReading', () => {
  it('prefiere lo que llegó en vivo', () => {
    const reading = currentReading(metrics(), [
      live('2026-09-23T12:00:20Z', {
        disks: [disk('2026-09-23T12:00:20Z', gib, '/')],
        networks: [network('2026-09-23T12:00:20Z', 10, 'eth0')],
      }),
    ])

    expect(reading.sample?.cpu_used_percent).toBe(80)
    expect(reading.disks).toEqual([
      { mountpoint: '/', point: disk('2026-09-23T12:00:20Z', gib, '/') },
    ])
    expect(reading.networks[0].interface).toBe('eth0')
  })

  it('sin canal en vivo usa el último punto de la serie', () => {
    const reading = currentReading(
      metrics({
        samples: [sampleAt('2026-09-23T12:00:00Z', { cpu_used_percent: 42 })],
        disks: [{ mountpoint: '/', points: [disk('2026-09-23T12:00:00Z', gib)] }],
      }),
      [],
    )

    expect(reading.sample?.cpu_used_percent).toBe(42)
    expect(reading.disks[0].mountpoint).toBe('/')
  })

  it('sin nada devuelve todo vacío', () => {
    expect(currentReading(undefined, [])).toEqual({ sample: null, disks: [], networks: [] })
  })
})
