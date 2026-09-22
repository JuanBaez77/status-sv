package api

import (
	"bytes"
	"slices"
	"sync"

	"github.com/google/uuid"
)

// sendBuffer es cuántos mensajes se le guardan a un panel que no lee. Con una
// muestra cada 10 s por servidor, 32 es más de cinco minutos de atraso.
const sendBuffer = 32

// maxSubscriptions acota a cuántos servidores puede suscribirse un panel. Es
// muy por encima de lo que hay, y evita que una conexión guarde una lista sin
// límite.
const maxSubscriptions = 500

// panelClient es una pestaña del panel conectada por WebSocket.
type panelClient struct {
	send chan []byte
	// done se cierra cuando el control plane apaga la conexión.
	done chan struct{}

	mu      sync.Mutex
	servers map[uuid.UUID]struct{}
	closed  bool
	dropped int
}

func newPanelClient() *panelClient {
	return &panelClient{
		send:    make(chan []byte, sendBuffer),
		done:    make(chan struct{}),
		servers: make(map[uuid.UUID]struct{}),
	}
}

// subscribe agrega servidores a lo que este panel quiere ver.
func (c *panelClient) subscribe(ids []uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		if len(c.servers) >= maxSubscriptions {
			return
		}
		c.servers[id] = struct{}{}
	}
}

func (c *panelClient) unsubscribe(ids []uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		delete(c.servers, id)
	}
}

func (c *panelClient) subscribed(id uuid.UUID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.servers[id]
	return ok
}

// offer deja un mensaje para enviar. Si el panel no lee y el buffer se llenó,
// el mensaje se descarta: el que llega tarde ya no sirve, y el panel se pone al
// día con la API REST. Devuelve false cuando descartó.
func (c *panelClient) offer(message []byte) bool {
	select {
	case c.send <- message:
		return true
	default:
		c.mu.Lock()
		c.dropped++
		c.mu.Unlock()
		return false
	}
}

// close le avisa al escritor que termine. Es idempotente.
func (c *panelClient) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
}

func (c *panelClient) droppedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

// panelHub reparte las novedades entre los paneles conectados.
type panelHub struct {
	mu      sync.Mutex
	next    uint64
	clients map[uint64]*panelClient
}

func newPanelHub() *panelHub {
	return &panelHub{clients: make(map[uint64]*panelClient)}
}

func (h *panelHub) add(client *panelClient) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	h.clients[h.next] = client
	return h.next
}

func (h *panelHub) remove(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, id)
}

func (h *panelHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// publish le manda un mensaje a los paneles suscriptos a ese servidor, y
// devuelve a cuántos les llegó.
func (h *panelHub) publish(serverID uuid.UUID, message []byte) int {
	h.mu.Lock()
	clients := make([]*panelClient, 0, len(h.clients))
	for _, client := range h.clients {
		clients = append(clients, client)
	}
	h.mu.Unlock()

	sent := 0
	for _, client := range clients {
		if client.subscribed(serverID) && client.offer(message) {
			sent++
		}
	}
	return sent
}

// hasSubscribers dice si alguien está mirando ese servidor. Sirve para no armar
// un mensaje que no va a ir a ningún lado.
func (h *panelHub) hasSubscribers(serverID uuid.UUID) bool {
	h.mu.Lock()
	clients := make([]*panelClient, 0, len(h.clients))
	for _, client := range h.clients {
		clients = append(clients, client)
	}
	h.mu.Unlock()

	for _, client := range clients {
		if client.subscribed(serverID) {
			return true
		}
	}
	return false
}

// closeAll cierra todas las conexiones del panel. Se usa al apagar.
func (h *panelHub) closeAll() {
	h.mu.Lock()
	clients := make([]*panelClient, 0, len(h.clients))
	for _, client := range h.clients {
		clients = append(clients, client)
	}
	h.mu.Unlock()

	for _, client := range clients {
		client.close()
	}
}

// list devuelve los servidores a los que está suscripto, para confirmárselo al
// panel.
func (c *panelClient) list() []uuid.UUID {
	c.mu.Lock()
	defer c.mu.Unlock()

	ids := make([]uuid.UUID, 0, len(c.servers))
	for id := range c.servers {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return ids
}
