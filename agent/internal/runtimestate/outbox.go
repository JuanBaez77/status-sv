package runtimestate

import (
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

// AckTimeout es cuánto se espera el acuse de una foto antes de darla por
// perdida y volver a mandarla, como con las métricas.
const AckTimeout = 30 * time.Second

// Outbox es la bandeja de salida de las fotos. La llena el Runner y la vacía
// el canal con el control plane, cada uno en su goroutine.
//
// Hay una sola foto en vuelo, y lo que espera es solo la más nueva: una foto
// entera reemplaza a todas las anteriores, así que no tiene sentido mandar una
// vieja si ya hay otra. Por eso la bandeja no crece nunca.
type Outbox struct {
	mu sync.Mutex
	// pending es la próxima foto por mandar.
	pending *proto.RuntimeState
	// inflight es la foto que se mandó y espera acuse, con el ID del mensaje
	// para reconocer un rechazo y cuándo salió para reconocer un silencio.
	inflight   *proto.RuntimeState
	inflightID uuid.UUID
	sentAt     time.Time

	wake chan struct{}
}

// NewOutbox arma una bandeja vacía.
func NewOutbox() *Outbox {
	return &Outbox{wake: make(chan struct{}, 1)}
}

// Put deja una foto para mandar, en lugar de la que estuviera esperando.
func (o *Outbox) Put(s proto.RuntimeState) {
	o.mu.Lock()
	o.pending = &s
	o.mu.Unlock()
	o.signal()
}

// Next devuelve la foto que hay que mandar ahora, si hay una. No hay nada si la
// anterior todavía espera su acuse, salvo que se haya pasado AckTimeout: en ese
// caso vuelve a salir (o sale la más nueva, si llegó otra).
func (o *Outbox) Next(now time.Time) (proto.RuntimeState, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.inflight != nil {
		if now.Sub(o.sentAt) < AckTimeout {
			return proto.RuntimeState{}, false
		}
		o.requeueLocked()
	}
	if o.pending == nil {
		return proto.RuntimeState{}, false
	}
	return *o.pending, true
}

// Sent registra que la foto tomada en takenAt salió en el mensaje id. Hay que
// llamarlo antes de escribir en el canal: el acuse puede llegar antes de que
// termine la escritura.
func (o *Outbox) Sent(id uuid.UUID, takenAt, now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pending == nil || !o.pending.TakenAt.Equal(takenAt) {
		return
	}
	o.inflight, o.pending = o.pending, nil
	o.inflightID, o.sentAt = id, now
}

// Acked registra el acuse de la foto tomada en takenAt.
func (o *Outbox) Acked(takenAt time.Time) {
	o.mu.Lock()
	if o.inflight != nil && o.inflight.TakenAt.Equal(takenAt) {
		o.inflight = nil
	}
	// Un acuse que llega tarde, de una foto que ya se había dado por perdida y
	// volvió a la espera: el control plane ya la tiene.
	if o.pending != nil && !o.pending.TakenAt.After(takenAt) {
		o.pending = nil
	}
	pending := o.pending != nil
	o.mu.Unlock()
	if pending {
		o.signal()
	}
}

// Rejected registra que el control plane rechazó el mensaje id. Devuelve si
// era el de la foto en vuelo. Si el rechazo es reintentable, la foto vuelve a
// esperar (salvo que ya haya una más nueva); si no, se descarta.
func (o *Outbox) Rejected(id uuid.UUID, retryable bool) bool {
	o.mu.Lock()
	if o.inflight == nil || o.inflightID != id {
		o.mu.Unlock()
		return false
	}
	if retryable {
		o.requeueLocked()
	} else {
		o.inflight = nil
	}
	o.mu.Unlock()
	return true
}

// Restart se llama al abrir un canal nuevo: el acuse de la foto en vuelo en el
// canal anterior no va a llegar, así que vuelve a esperar.
func (o *Outbox) Restart() {
	o.mu.Lock()
	o.requeueLocked()
	pending := o.pending != nil
	o.mu.Unlock()
	if pending {
		o.signal()
	}
}

// Wake avisa cuando puede haber algo para mandar.
func (o *Outbox) Wake() <-chan struct{} {
	return o.wake
}

// requeueLocked devuelve la foto en vuelo a la espera, salvo que ya haya una
// más nueva, que la reemplaza.
func (o *Outbox) requeueLocked() {
	if o.inflight == nil {
		return
	}
	if o.pending == nil {
		o.pending = o.inflight
	}
	o.inflight = nil
}

func (o *Outbox) signal() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}
