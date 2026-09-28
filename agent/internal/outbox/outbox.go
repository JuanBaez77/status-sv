// Package outbox es la bandeja de salida de los mensajes que viajan como foto
// entera: el estado de runtime y los proyectos declarados. Una foto nueva
// reemplaza a todas las anteriores, así que la bandeja guarda solo la próxima
// por mandar y la que espera acuse, y no crece nunca.
package outbox

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// AckTimeout es cuánto se espera el acuse de una foto antes de darla por
// perdida y volver a mandarla, como con las métricas.
const AckTimeout = 30 * time.Second

// Box es la bandeja de salida de un tipo de foto. La llena quien toma las
// fotos y la vacía el canal con el control plane, cada uno en su goroutine.
//
// Hay una sola foto en vuelo, y lo que espera es solo la más nueva. Cada foto
// se reconoce por su marca de tiempo, que es también lo que nombra el acuse.
type Box[T any] struct {
	stamp func(T) time.Time

	mu sync.Mutex
	// pending es la próxima foto por mandar.
	pending *T
	// inflight es la foto que se mandó y espera acuse, con el ID del mensaje
	// para reconocer un rechazo y cuándo salió para reconocer un silencio.
	inflight   *T
	inflightID uuid.UUID
	sentAt     time.Time

	wake chan struct{}
}

// New arma una bandeja vacía. stamp devuelve la marca de tiempo de una foto.
func New[T any](stamp func(T) time.Time) *Box[T] {
	return &Box[T]{stamp: stamp, wake: make(chan struct{}, 1)}
}

// Put deja una foto para mandar, en lugar de la que estuviera esperando.
func (b *Box[T]) Put(s T) {
	b.mu.Lock()
	b.pending = &s
	b.mu.Unlock()
	b.signal()
}

// Next devuelve la foto que hay que mandar ahora, si hay una. No hay nada si la
// anterior todavía espera su acuse, salvo que se haya pasado AckTimeout: en ese
// caso vuelve a salir (o sale la más nueva, si llegó otra).
func (b *Box[T]) Next(now time.Time) (T, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var zero T
	if b.inflight != nil {
		if now.Sub(b.sentAt) < AckTimeout {
			return zero, false
		}
		b.requeueLocked()
	}
	if b.pending == nil {
		return zero, false
	}
	return *b.pending, true
}

// Sent registra que la foto tomada en takenAt salió en el mensaje id. Hay que
// llamarlo antes de escribir en el canal: el acuse puede llegar antes de que
// termine la escritura.
func (b *Box[T]) Sent(id uuid.UUID, takenAt, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending == nil || !b.stamp(*b.pending).Equal(takenAt) {
		return
	}
	b.inflight, b.pending = b.pending, nil
	b.inflightID, b.sentAt = id, now
}

// Acked registra el acuse de la foto tomada en takenAt.
func (b *Box[T]) Acked(takenAt time.Time) {
	b.mu.Lock()
	if b.inflight != nil && b.stamp(*b.inflight).Equal(takenAt) {
		b.inflight = nil
	}
	// Un acuse que llega tarde, de una foto que ya se había dado por perdida y
	// volvió a la espera: el control plane ya la tiene.
	if b.pending != nil && !b.stamp(*b.pending).After(takenAt) {
		b.pending = nil
	}
	pending := b.pending != nil
	b.mu.Unlock()
	if pending {
		b.signal()
	}
}

// Rejected registra que el control plane rechazó el mensaje id. Devuelve si
// era el de la foto en vuelo. Si el rechazo es reintentable, la foto vuelve a
// esperar (salvo que ya haya una más nueva); si no, se descarta.
func (b *Box[T]) Rejected(id uuid.UUID, retryable bool) bool {
	b.mu.Lock()
	if b.inflight == nil || b.inflightID != id {
		b.mu.Unlock()
		return false
	}
	if retryable {
		b.requeueLocked()
	} else {
		b.inflight = nil
	}
	b.mu.Unlock()
	return true
}

// Restart se llama al abrir un canal nuevo: el acuse de la foto en vuelo en el
// canal anterior no va a llegar, así que vuelve a esperar.
func (b *Box[T]) Restart() {
	b.mu.Lock()
	b.requeueLocked()
	pending := b.pending != nil
	b.mu.Unlock()
	if pending {
		b.signal()
	}
}

// Wake avisa cuando puede haber algo para mandar.
func (b *Box[T]) Wake() <-chan struct{} {
	return b.wake
}

// Stamp devuelve la marca de tiempo de una foto.
func (b *Box[T]) Stamp(s T) time.Time {
	return b.stamp(s)
}

// requeueLocked devuelve la foto en vuelo a la espera, salvo que ya haya una
// más nueva, que la reemplaza.
func (b *Box[T]) requeueLocked() {
	if b.inflight == nil {
		return
	}
	if b.pending == nil {
		b.pending = b.inflight
	}
	b.inflight = nil
}

func (b *Box[T]) signal() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}
