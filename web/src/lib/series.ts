/**
 * De lo que devuelve la API a lo que dibuja un gráfico.
 *
 * Todo lo de este archivo es cálculo puro sobre los datos: así se puede probar
 * sin navegador y el componente del gráfico se queda solo con el dibujo.
 */

import type { DiskPoint, NetworkPoint, NetworkSeries, SamplePoint, ServerMetrics } from '@/lib/api'
import type { LiveSample } from '@/lib/panel-socket'

/** ChartColor es un color del tema, no un color suelto: lo resuelve el gráfico. */
export type ChartColor = 'chart-1' | 'chart-2' | 'chart-3' | 'chart-4' | 'chart-5'

/** ChartLine es una línea ya lista para dibujar. */
export type ChartLine = {
  label: string
  color: ChartColor
  /** values tiene un valor por cada instante de ChartData.x; null es un hueco. */
  values: (number | null)[]
  /** dashed marca las líneas de referencia (los picos), que van punteadas. */
  dashed?: boolean
}

export type ChartData = {
  /** x son los instantes en segundos, que es lo que come uPlot. */
  x: number[]
  lines: ChartLine[]
}

/**
 * gapFloor es el hueco más chico que se dibuja como corte, en segundos.
 *
 * Un agent que manda cada 10 s y se demora un poco no debería llenar el gráfico
 * de cortes; uno que estuvo dos minutos caído, sí: una recta entre dos puntos
 * lejanos diría que en el medio pasó algo que nadie midió.
 */
const gapFloor = 120

function toSeconds(iso: string): number {
  return Math.round(Date.parse(iso) / 1000)
}

/**
 * axisFrom arma el eje de tiempos de un gráfico y le agrega un instante vacío
 * en cada hueco, para que el dibujo se corte en vez de unir dos puntos lejanos
 * con una recta.
 *
 * Los extremos del rango consultado entran siempre, aunque no haya ningún dato
 * ahí: así todos los gráficos de un servidor abarcan la misma ventana y un pico
 * de CPU se puede comparar con el de la red mirando la misma posición.
 */
function axisFrom(range: { from: string; to: string }, lists: { ts: string }[][]): number[] {
  const unique = new Set<number>([toSeconds(range.from), toSeconds(range.to)])
  for (const list of lists) {
    for (const point of list) {
      unique.add(toSeconds(point.ts))
    }
  }
  return withGaps([...unique].sort((a, b) => a - b))
}

function withGaps(sorted: number[]): number[] {
  if (sorted.length < 3) return sorted

  const steps = sorted.slice(1).map((value, i) => value - sorted[i])
  const median = [...steps].sort((a, b) => a - b)[Math.floor(steps.length / 2)]
  const threshold = Math.max(median * 3, gapFloor)

  const axis: number[] = []
  for (const [i, value] of sorted.entries()) {
    if (i > 0 && value - sorted[i - 1] > threshold) {
      axis.push(sorted[i - 1] + Math.max(Math.round(median), 1))
    }
    axis.push(value)
  }
  return axis
}

/** onAxis pone una serie sobre el eje común; lo que no tiene queda en null. */
function onAxis<T extends { ts: string }>(
  x: number[],
  points: T[],
  value: (point: T) => number,
): (number | null)[] {
  const byTime = new Map<number, number>()
  for (const point of points) {
    byTime.set(toSeconds(point.ts), value(point))
  }
  return x.map((time) => byTime.get(time) ?? null)
}

/**
 * mergeLive suma al final de las series las muestras que llegaron por el canal
 * en vivo.
 *
 * Solo se hace en resolución cruda. En 1m cada punto es el promedio de un
 * minuto: pegarle atrás muestras sueltas mezclaría dos cosas distintas en la
 * misma línea, y además el minuto en curso se recalcula solo en cada consulta.
 */
export function mergeLive(metrics: ServerMetrics, live: LiveSample[]): ServerMetrics {
  if (metrics.resolution !== 'raw' || live.length === 0) return metrics

  const last = metrics.samples.at(-1)
  const after = last === undefined ? Date.parse(metrics.from) : Date.parse(last.ts)
  const fresh = live.filter((entry) => Date.parse(entry.sample.ts) > after)
  if (fresh.length === 0) return metrics

  const disks = metrics.disks.map((series) => ({ ...series, points: [...series.points] }))
  const networks = metrics.networks.map((series) => ({ ...series, points: [...series.points] }))

  for (const entry of fresh) {
    for (const disk of entry.disks) {
      appendPoint(disks, 'mountpoint', disk.mountpoint, disk)
    }
    for (const network of entry.networks) {
      appendPoint(networks, 'interface', network.interface, network)
    }
  }

  return {
    ...metrics,
    to: fresh[fresh.length - 1].sample.ts,
    samples: [...metrics.samples, ...fresh.map((entry) => entry.sample)],
    disks,
    networks,
  }
}

/** appendPoint suma un punto a la serie que le corresponde, creándola si es nueva. */
function appendPoint<K extends string, P>(
  series: (Record<K, string> & { points: P[] })[],
  key: K,
  name: string | undefined,
  point: P,
) {
  if (name === undefined) return

  const found = series.find((entry) => entry[key] === name)
  if (found === undefined) {
    series.push({ [key]: name, points: [point] } as Record<K, string> & { points: P[] })
    return
  }
  found.points.push(point)
}

/** Reading es el estado de ahora: lo último que se sabe del servidor. */
export type Reading = {
  sample: SamplePoint | null
  disks: { mountpoint: string; point: DiskPoint }[]
  networks: { interface: string; point: NetworkPoint }[]
}

/**
 * currentReading es lo que muestran los números de arriba: la última muestra
 * que llegó en vivo o, si todavía no llegó ninguna, la última de la serie.
 */
export function currentReading(metrics: ServerMetrics | undefined, live: LiveSample[]): Reading {
  const latest = live.at(-1)
  if (latest !== undefined) {
    return {
      sample: latest.sample,
      disks: latest.disks.flatMap((point) =>
        point.mountpoint === undefined ? [] : [{ mountpoint: point.mountpoint, point }],
      ),
      networks: latest.networks.flatMap((point) =>
        point.interface === undefined ? [] : [{ interface: point.interface, point }],
      ),
    }
  }

  if (metrics === undefined) {
    return { sample: null, disks: [], networks: [] }
  }

  return {
    sample: metrics.samples.at(-1) ?? null,
    disks: metrics.disks.flatMap((series) => {
      const point = series.points.at(-1)
      return point === undefined ? [] : [{ mountpoint: series.mountpoint, point }]
    }),
    networks: metrics.networks.flatMap((series) => {
      const point = series.points.at(-1)
      return point === undefined ? [] : [{ interface: series.interface, point }]
    }),
  }
}

export function cpuChart(metrics: ServerMetrics): ChartData {
  const x = axisFrom(metrics, [metrics.samples])
  const lines: ChartLine[] = [
    { label: 'Uso', color: 'chart-1', values: onAxis(x, metrics.samples, (s) => s.cpu_used_percent) },
  ]
  // En crudo el pico es la misma muestra: repetir la línea no diría nada.
  if (metrics.resolution === '1m') {
    lines.push({
      label: 'Pico',
      color: 'chart-1',
      dashed: true,
      values: onAxis(x, metrics.samples, (s) => s.cpu_used_max_percent),
    })
  }
  lines.push({
    label: 'Espera de E/S',
    color: 'chart-3',
    values: onAxis(x, metrics.samples, (s) => s.cpu_iowait_percent),
  })
  return { x, lines }
}

export function memoryChart(metrics: ServerMetrics): ChartData {
  const x = axisFrom(metrics, [metrics.samples])
  const lines: ChartLine[] = [
    { label: 'En uso', color: 'chart-1', values: onAxis(x, metrics.samples, (s) => s.mem_used_bytes) },
  ]
  if (metrics.resolution === '1m') {
    lines.push({
      label: 'Pico',
      color: 'chart-1',
      dashed: true,
      values: onAxis(x, metrics.samples, (s) => s.mem_used_max_bytes),
    })
  }
  // El swap solo aparece si el servidor tiene: en la mayoría sería una línea en cero.
  if (metrics.samples.some((s) => s.swap_total_bytes > 0)) {
    lines.push({
      label: 'Swap',
      color: 'chart-4',
      values: onAxis(x, metrics.samples, (s) => s.swap_used_bytes),
    })
  }
  return { x, lines }
}

export function loadChart(metrics: ServerMetrics): ChartData {
  const x = axisFrom(metrics, [metrics.samples])
  return {
    x,
    lines: [
      { label: '1 min', color: 'chart-1', values: onAxis(x, metrics.samples, (s) => s.load1) },
      { label: '5 min', color: 'chart-2', values: onAxis(x, metrics.samples, (s) => s.load5) },
      { label: '15 min', color: 'chart-5', values: onAxis(x, metrics.samples, (s) => s.load15) },
    ],
  }
}

const diskColors: ChartColor[] = ['chart-1', 'chart-2', 'chart-3', 'chart-5', 'chart-4']

/** diskChart dibuja el uso de cada punto de montaje, en porcentaje. */
export function diskChart(metrics: ServerMetrics): ChartData {
  const x = axisFrom(metrics, metrics.disks.map((series) => series.points))
  return {
    x,
    lines: metrics.disks.map((series, i) => ({
      label: series.mountpoint,
      color: diskColors[i % diskColors.length],
      values: onAxis(x, series.points, (point) =>
        point.total_bytes > 0 ? (point.used_bytes / point.total_bytes) * 100 : 0,
      ),
    })),
  }
}

/** networkChart dibuja una interfaz: lo que entra y lo que sale, por segundo. */
export function networkChart(metrics: ServerMetrics, series: NetworkSeries): ChartData {
  const x = axisFrom(metrics, [series.points])
  return {
    x,
    lines: [
      {
        label: 'Entrada',
        color: 'chart-2',
        values: onAxis(x, series.points, (p) => p.rx_bytes_per_sec),
      },
      {
        label: 'Salida',
        color: 'chart-5',
        values: onAxis(x, series.points, (p) => p.tx_bytes_per_sec),
      },
    ],
  }
}
