/**
 * Canal en vivo del panel.
 *
 * El control plane empuja cada muestra apenas la guarda, pero solo de los
 * servidores a los que el panel se suscribió: una pestaña abierta en un
 * servidor no paga por los demás.
 *
 * Toda la pestaña comparte un WebSocket. Las vistas se suscriben a lo que están
 * mirando y se dan de baja al salir; la conexión queda abierta aunque no quede
 * ninguna suscripción, porque volver a abrirla en cada navegación cuesta más
 * que un ping cada 30 segundos.
 *
 * La sesión viaja en la cookie, que el navegador manda sola en el saludo
 * inicial: acá no se guarda ningún token (ADR-010).
 */

import type { DiskPoint, NetworkPoint, SamplePoint } from '@/lib/api'

/** LiveSample es una muestra recién guardada, con la misma forma que los puntos de la API REST. */
export type LiveSample = {
  server_id: string
  sample: SamplePoint
  disks: DiskPoint[]
  networks: NetworkPoint[]
}

/** PanelStatus dice si lo que se ve es de ahora. */
export type PanelStatus = 'connecting' | 'live' | 'reconnecting'

type SampleListener = (sample: LiveSample) => void
type StatusListener = (status: PanelStatus) => void

/**
 * retryDelays son las esperas entre reintentos, en milisegundos. Van creciendo
 * para no golpear a un control plane que se está reiniciando.
 *
 * Sin azar a propósito: los paneles abiertos son unos pocos, así que no hay una
 * avalancha que repartir como sí la hay con los agents.
 */
const retryDelays = [1_000, 2_000, 5_000, 10_000, 30_000]

export class PanelSocket {
  #open: () => WebSocket
  #socket: WebSocket | null = null
  #status: PanelStatus = 'connecting'
  #listeners = new Map<string, Set<SampleListener>>()
  #statusListeners = new Set<StatusListener>()
  #attempt = 0
  #retry: ReturnType<typeof setTimeout> | null = null

  constructor(open: () => WebSocket = openPanelSocket) {
    this.#open = open
  }

  /**
   * subscribe empieza a recibir las muestras de un servidor y devuelve cómo
   * darse de baja.
   */
  subscribe(serverID: string, listener: SampleListener): () => void {
    const existing = this.#listeners.get(serverID)
    if (existing === undefined) {
      this.#listeners.set(serverID, new Set([listener]))
      this.#send({ type: 'subscribe', server_ids: [serverID] })
    } else {
      existing.add(listener)
    }
    this.#connect()

    return () => {
      const listeners = this.#listeners.get(serverID)
      if (listeners === undefined) return
      listeners.delete(listener)
      if (listeners.size === 0) {
        this.#listeners.delete(serverID)
        this.#send({ type: 'unsubscribe', server_ids: [serverID] })
      }
    }
  }

  /** getStatus y onStatus son el par que espera useSyncExternalStore. */
  getStatus = (): PanelStatus => this.#status

  onStatus = (listener: StatusListener): (() => void) => {
    this.#statusListeners.add(listener)
    return () => this.#statusListeners.delete(listener)
  }

  /** close corta el canal y olvida las suscripciones: es lo que hace el cierre de sesión. */
  close() {
    if (this.#retry !== null) {
      clearTimeout(this.#retry)
      this.#retry = null
    }
    this.#listeners.clear()
    this.#attempt = 0
    this.#setStatus('connecting')

    const socket = this.#socket
    this.#socket = null
    socket?.close(1000, 'el panel se está cerrando')
  }

  #connect() {
    if (this.#socket !== null || this.#retry !== null) return

    const socket = this.#open()
    this.#socket = socket

    socket.onopen = () => {
      this.#attempt = 0
      const ids = [...this.#listeners.keys()]
      if (ids.length > 0) {
        this.#send({ type: 'subscribe', server_ids: ids })
      }
    }
    socket.onmessage = (event: MessageEvent) => this.#receive(event.data)
    socket.onclose = () => {
      if (this.#socket !== socket) return // ya lo cerramos nosotros
      this.#socket = null
      this.#scheduleRetry()
    }
    // Un error siempre viene seguido de un cierre, así que el reintento se
    // maneja en un solo lugar.
    socket.onerror = () => {}
  }

  #scheduleRetry() {
    if (this.#listeners.size === 0) {
      // Nadie está mirando: la conexión se vuelve a abrir cuando alguien se suscriba.
      this.#setStatus('connecting')
      return
    }

    this.#setStatus('reconnecting')
    const delay = retryDelays[Math.min(this.#attempt, retryDelays.length - 1)]
    this.#attempt++
    this.#retry = setTimeout(() => {
      this.#retry = null
      this.#connect()
    }, delay)
  }

  #receive(data: unknown) {
    if (typeof data !== 'string') return

    let message: { type?: string; server_id?: string; message?: string }
    try {
      message = JSON.parse(data)
    } catch {
      return
    }

    switch (message.type) {
      case 'subscribed':
        // El control plane confirmó la suscripción: recién ahora van a llegar muestras.
        this.#setStatus('live')
        break
      case 'sample': {
        const sample = message as LiveSample
        this.#listeners.get(sample.server_id)?.forEach((listener) => listener(sample))
        break
      }
      case 'error':
        console.warn('el control plane rechazó un mensaje del panel:', message.message)
        break
      default:
      // welcome y lo que venga más adelante no cambian nada de lo que se ve.
    }
  }

  #send(request: { type: string; server_ids: string[] }) {
    if (this.#socket?.readyState !== WebSocket.OPEN) return
    this.#socket.send(JSON.stringify(request))
  }

  #setStatus(status: PanelStatus) {
    if (this.#status === status) return
    this.#status = status
    this.#statusListeners.forEach((listener) => listener(status))
  }
}

function openPanelSocket(): WebSocket {
  const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  return new WebSocket(`${scheme}//${window.location.host}/v1/panel/connect`)
}

/** panelSocket es el canal que comparte toda la pestaña. */
export const panelSocket = new PanelSocket()
