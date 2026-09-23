import { useEffect, useRef } from 'react'
import uPlot from 'uplot'
import 'uplot/dist/uPlot.min.css'

import type { ChartColor, ChartData } from '@/lib/series'

/**
 * Chart dibuja una serie de tiempo con uPlot.
 *
 * uPlot dibuja en un canvas, así que no hay un nodo por punto: un gráfico de
 * una hora en crudo son 360 puntos por línea y varios gráficos en la misma
 * pantalla. A cambio, el canvas no le dice nada a un lector de pantalla, así
 * que el contenedor lleva un resumen en texto con el último valor de cada
 * línea.
 */
type ChartProps = {
  /** label es de qué es el gráfico: va en el resumen que lee un lector de pantalla. */
  label: string
  data: ChartData
  /**
   * format arma los números del eje y de la leyenda (porcentaje, bytes,
   * bytes/s…). Tiene que ser una función estable: si cambia en cada render, el
   * gráfico se rehace en cada render.
   */
  format: (value: number) => string
  /** max fija el techo del eje: en los porcentajes es 100, para que la escala no exagere. */
  max?: number
  height?: number
}

/** Colores de reserva, por si el gráfico se dibuja antes de que carguen los estilos. */
const fallbackColors: Record<ChartColor, string> = {
  'chart-1': '#60a5fa',
  'chart-2': '#4ade80',
  'chart-3': '#fbbf24',
  'chart-4': '#f87171',
  'chart-5': '#c084fc',
}

const dateNames = {
  MMMM: [
    'enero',
    'febrero',
    'marzo',
    'abril',
    'mayo',
    'junio',
    'julio',
    'agosto',
    'septiembre',
    'octubre',
    'noviembre',
    'diciembre',
  ],
  MMM: ['ene', 'feb', 'mar', 'abr', 'may', 'jun', 'jul', 'ago', 'sep', 'oct', 'nov', 'dic'],
  WWWW: ['domingo', 'lunes', 'martes', 'miércoles', 'jueves', 'viernes', 'sábado'],
  WWW: ['dom', 'lun', 'mar', 'mié', 'jue', 'vie', 'sáb'],
}

export function Chart({ label, data, format, max, height = 180 }: ChartProps) {
  const holder = useRef<HTMLDivElement>(null)
  const plot = useRef<uPlot | null>(null)

  // El dibujo se rehace solo cuando cambian las líneas; los datos nuevos entran
  // por setData, que uPlot resuelve sin volver a armar todo.
  const shape = data.lines
    .map((line) => `${line.label}/${line.color}/${line.dashed ?? false}`)
    .join('|')

  // Los datos nuevos se le pasan al gráfico que ya existe, y quedan a mano para
  // el que se cree después: este efecto está declarado antes a propósito, así
  // React lo corre primero cuando cambian las dos cosas en el mismo render.
  const latest = useRef(data)
  useEffect(() => {
    latest.current = data
    plot.current?.setData(toAligned(data))
  }, [data])

  useEffect(() => {
    const node = holder.current
    if (node === null) return

    const current = latest.current
    const chart = new uPlot(
      {
        width: node.clientWidth || 600,
        height,
        fmtDate: (tpl) => uPlot.fmtDate(tpl, dateNames),
        legend: { show: true, live: true },
        cursor: { y: false, points: { size: 5 } },
        scales: { y: { range: (_u, min, maxValue) => [0, max ?? Math.max(maxValue, min, 0.001)] } },
        axes: [
          { stroke: themeColor('chart-axis', '#9ca3af'), grid: { show: false } },
          {
            stroke: themeColor('chart-axis', '#9ca3af'),
            grid: { stroke: themeColor('chart-grid', '#374151'), width: 1 },
            // Espacio para etiquetas largas: "953,7 MiB" no entra en menos.
            size: 72,
            values: (_u, splits) => splits.map((value) => format(value)),
          },
        ],
        series: [
          { label: 'Hora' },
          ...current.lines.map((line) => ({
            label: line.label,
            stroke: themeColor(line.color, fallbackColors[line.color]),
            width: 1.5,
            dash: line.dashed === true ? [4, 4] : undefined,
            value: (_u: uPlot, value: number | null) => (value === null ? '—' : format(value)),
          })),
        ],
      },
      toAligned(current),
      node,
    )
    plot.current = chart

    const observer = new ResizeObserver(([entry]) => {
      chart.setSize({ width: Math.round(entry.contentRect.width), height })
    })
    observer.observe(node)

    return () => {
      observer.disconnect()
      chart.destroy()
      plot.current = null
    }
  }, [shape, height, max, format])

  return (
    <div
      ref={holder}
      role="img"
      aria-label={`${label}. ${summarize(data, format)}`}
      className="w-full [&_.u-legend]:text-xs [&_.u-legend]:text-muted-foreground"
    />
  )
}

function toAligned(data: ChartData): uPlot.AlignedData {
  return [data.x, ...data.lines.map((line) => line.values)] as uPlot.AlignedData
}

/** summarize es lo que escucha quien no ve el gráfico: el último valor de cada línea. */
function summarize(data: ChartData, format: (value: number) => string): string {
  if (data.x.length === 0) return 'Sin datos en este rango.'

  const parts = data.lines.map((line) => {
    const last = [...line.values].reverse().find((value) => value !== null)
    return `${line.label}: ${last === undefined || last === null ? 'sin datos' : format(last)}`
  })
  return `Último valor. ${parts.join('. ')}.`
}

/**
 * themeColor lee un color del tema. El canvas no entiende variables de CSS, así
 * que hay que resolverlas a mano al crear el gráfico.
 */
function themeColor(token: string, fallback: string): string {
  const value = getComputedStyle(document.documentElement).getPropertyValue(`--${token}`).trim()
  return value === '' ? fallback : value
}
