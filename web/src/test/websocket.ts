/**
 * Un WebSocket de mentira para las pruebas: no abre nada y deja decidir cuándo
 * se conecta, qué llega y cuándo se corta.
 */
export class FakeWebSocket {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSING = 2
  static readonly CLOSED = 3

  /** sockets son todos los que se crearon, en orden: el último es el vigente. */
  static sockets: FakeWebSocket[] = []

  readyState: number = FakeWebSocket.CONNECTING
  /** sent son los mensajes que mandó el panel, sin parsear. */
  sent: string[] = []
  closedWith: number | undefined

  onopen: (() => void) | null = null
  onmessage: ((event: { data: string }) => void) | null = null
  onclose: (() => void) | null = null
  onerror: (() => void) | null = null

  readonly url: string

  constructor(url: string) {
    this.url = url
    FakeWebSocket.sockets.push(this)
  }

  send(data: string) {
    this.sent.push(data)
  }

  close(code?: number) {
    this.readyState = FakeWebSocket.CLOSED
    this.closedWith = code
    this.onclose?.()
  }

  /** requests son los mensajes que mandó el panel, ya parseados. */
  get requests(): { type: string; server_ids: string[] }[] {
    return this.sent.map((raw) => JSON.parse(raw))
  }

  // De acá para abajo, lo que maneja la prueba.

  /** connect simula que el control plane aceptó la conexión. */
  connect() {
    this.readyState = FakeWebSocket.OPEN
    this.onopen?.()
  }

  /** receive simula un mensaje del control plane. */
  receive(message: unknown) {
    this.onmessage?.({ data: JSON.stringify(message) })
  }

  /** drop simula que la conexión se cortó sola. */
  drop() {
    this.readyState = FakeWebSocket.CLOSED
    this.onclose?.()
  }

  static reset() {
    FakeWebSocket.sockets = []
  }

  static get last(): FakeWebSocket {
    const socket = FakeWebSocket.sockets.at(-1)
    if (socket === undefined) throw new Error('todavía no se abrió ninguna conexión')
    return socket
  }
}

/** openFake es la fábrica que se le pasa a PanelSocket en las pruebas. */
export function openFake(): WebSocket {
  return new FakeWebSocket('ws://panel.test/v1/panel/connect') as unknown as WebSocket
}
