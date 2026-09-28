package api

import (
	"sync"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// hub lleva la cuenta de qué agents están conectados, uno por servidor.
//
// Existe por un caso concreto: si se corta la red, el control plane puede tardar
// en enterarse y quedarse con una conexión muerta. Cuando el agent reconecta,
// la conexión vieja se cierra y queda la nueva, que es la que está viva.
type hub struct {
	mu    sync.Mutex
	conns map[uuid.UUID]*websocket.Conn
}

func newHub() *hub {
	return &hub{conns: make(map[uuid.UUID]*websocket.Conn)}
}

// add registra la conexión de un servidor y devuelve la anterior, si había, para
// que quien llama la cierre.
func (h *hub) add(serverID uuid.UUID, conn *websocket.Conn) (previous *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	previous = h.conns[serverID]
	h.conns[serverID] = conn
	return previous
}

// remove saca la conexión, pero solo si sigue siendo la registrada: si el agent
// ya reconectó, la nueva conexión no se toca.
func (h *hub) remove(serverID uuid.UUID, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if current, ok := h.conns[serverID]; ok && current == conn {
		delete(h.conns, serverID)
	}
}

// get devuelve la conexión de un servidor, o nil si su agent no está conectado.
func (h *hub) get(serverID uuid.UUID) *websocket.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[serverID]
}

// count devuelve cuántos agents hay conectados.
func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return len(h.conns)
}

// closeAll cierra todas las conexiones. Se usa al apagar el control plane.
func (h *hub) closeAll(reason string) {
	h.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(h.conns))
	for _, conn := range h.conns {
		conns = append(conns, conn)
	}
	h.conns = make(map[uuid.UUID]*websocket.Conn)
	h.mu.Unlock()

	for _, conn := range conns {
		_ = conn.Close(websocket.StatusGoingAway, reason)
	}
}

// closeQuietly cierra una conexión sin hacer ruido si ya estaba cerrada.
func closeQuietly(conn *websocket.Conn, status websocket.StatusCode, reason string) {
	if conn == nil {
		return
	}
	_ = conn.Close(status, reason)
}
