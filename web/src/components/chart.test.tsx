import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { Chart } from '@/components/chart'
import { percent } from '@/lib/format'
import type { ChartData } from '@/lib/series'

// uPlot dibuja en un canvas, que jsdom no tiene. Lo que se prueba acá es el
// pegamento: qué series se arman, cuándo se redibuja y cuándo se destruye.
type FakePlot = {
  options: Record<string, unknown>
  data: unknown
  setData: ReturnType<typeof vi.fn>
  destroy: ReturnType<typeof vi.fn>
}

const instances: FakePlot[] = []

vi.mock('uplot', () => {
  class FakeUPlot {
    options: Record<string, unknown>
    data: unknown
    setData = vi.fn()
    destroy = vi.fn()

    constructor(options: Record<string, unknown>, data: unknown) {
      this.options = options
      this.data = data
      instances.push(this as unknown as FakePlot)
    }

    static fmtDate = () => () => ''
  }
  return { default: FakeUPlot }
})

const data: ChartData = {
  x: [1_758_628_800, 1_758_628_810],
  lines: [
    { label: 'Uso', color: 'chart-1', values: [10, 20] },
    { label: 'Pico', color: 'chart-1', dashed: true, values: [40, null] },
  ],
}

beforeEach(() => {
  instances.length = 0
})

describe('Chart', () => {
  it('arma una serie por línea, más la del tiempo', () => {
    render(<Chart label="Uso de CPU" data={data} format={percent} max={100} />)

    const series = instances[0].options.series as { label: string; dash?: number[] }[]
    expect(series.map((entry) => entry.label)).toEqual(['Hora', 'Uso', 'Pico'])
    // El pico va punteado: dos líneas del mismo color tienen que distinguirse.
    expect(series[1].dash).toBeUndefined()
    expect(series[2].dash).toEqual([4, 4])
  })

  it('le pasa a uPlot los valores alineados con el tiempo', () => {
    render(<Chart label="Uso de CPU" data={data} format={percent} />)

    expect(instances[0].data).toEqual([data.x, [10, 20], [40, null]])
  })

  it('redibuja sin volver a crear el gráfico cuando llega un punto nuevo', () => {
    const { rerender } = render(<Chart label="Uso de CPU" data={data} format={percent} />)
    const creados = instances.length

    rerender(
      <Chart
        label="Uso de CPU"
        data={{
          x: [...data.x, 1_758_628_820],
          lines: data.lines.map((line) => ({ ...line, values: [...line.values, 30] })),
        }}
        format={percent}
      />,
    )

    expect(instances).toHaveLength(creados)
    expect(instances[0].setData).toHaveBeenCalled()
  })

  it('destruye el gráfico al desmontarse', () => {
    const { unmount } = render(<Chart label="Uso de CPU" data={data} format={percent} />)

    unmount()

    expect(instances[0].destroy).toHaveBeenCalled()
  })

  it('cuenta en texto lo que muestra, que el canvas no dice nada', () => {
    render(<Chart label="Uso de CPU" data={data} format={percent} />)

    // El último valor de cada línea; el de "Pico" es null y se toma el anterior.
    expect(screen.getByRole('img')).toHaveAccessibleName(
      'Uso de CPU. Último valor. Uso: 20 %. Pico: 40 %.',
    )
  })

  it('avisa cuando no hay nada que dibujar', () => {
    render(<Chart label="Uso de CPU" data={{ x: [], lines: [] }} format={percent} />)

    expect(screen.getByRole('img')).toHaveAccessibleName('Uso de CPU. Sin datos en este rango.')
  })
})
