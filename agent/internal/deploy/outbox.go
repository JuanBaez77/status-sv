package deploy

import (
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

// AckTimeout es cuánto se espera el acuse de una foto antes de volver a
// mandarla, como con el resto de las fotos.
const AckTimeout = 30 * time.Second

// Outbox es la bandeja de salida de las fotos de los deploys (ADR-015).
//
// A diferencia de las fotos de runtime, acá una foto nueva reemplaza solo a
// las anteriores del mismo deploy: la final de un deploy no se puede perder
// porque empiece otro. Guarda la última de cada deploy sin confirmar y manda
// de a una, la del deploy más viejo primero.
type Outbox struct {
	mu      sync.Mutex
	pending map[uuid.UUID]proto.DeployEvent
	// inflight es la foto que salió y espera acuse.
	inflight   *proto.DeployEvent
	inflightID uuid.UUID
	sentAt     time.Time

	wake chan struct{}
	// onAck se llama con cada acuse, fuera del candado: el motor anota en el
	// diario que la foto llegó.
	onAck func(id uuid.UUID, seq int)
}

// NewOutbox arma una bandeja vacía.
func NewOutbox() *Outbox {
	return &Outbox{pending: make(map[uuid.UUID]proto.DeployEvent), wake: make(chan struct{}, 1)}
}

func (o *Outbox) setOnAck(fn func(uuid.UUID, int)) {
	o.mu.Lock()
	o.onAck = fn
	o.mu.Unlock()
}

// Put deja una foto para mandar, en lugar de una más vieja del mismo deploy.
func (o *Outbox) Put(ev proto.DeployEvent) {
	o.mu.Lock()
	if old, ok := o.pending[ev.DeployID]; ok && old.Seq >= ev.Seq {
		o.mu.Unlock()
		return
	}
	o.pending[ev.DeployID] = ev
	o.mu.Unlock()
	o.signal()
}

// Next devuelve la foto que hay que mandar ahora, si hay una y no hay otra
// esperando acuse (salvo que se haya pasado AckTimeout).
func (o *Outbox) Next(now time.Time) (proto.DeployEvent, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.inflight != nil {
		if now.Sub(o.sentAt) < AckTimeout {
			return proto.DeployEvent{}, false
		}
		o.inflight = nil
	}
	if len(o.pending) == 0 {
		return proto.DeployEvent{}, false
	}
	events := make([]proto.DeployEvent, 0, len(o.pending))
	for _, ev := range o.pending {
		events = append(events, ev)
	}
	slices.SortFunc(events, func(a, b proto.DeployEvent) int {
		if c := a.StartedAt.Compare(b.StartedAt); c != 0 {
			return c
		}
		return slices.Compare(a.DeployID[:], b.DeployID[:])
	})
	return events[0], true
}

// Sent registra que la foto salió en el mensaje msgID. Hay que llamarlo antes
// de escribir en el canal: el acuse puede llegar antes de que termine.
func (o *Outbox) Sent(msgID uuid.UUID, ev proto.DeployEvent, now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.inflight = &ev
	o.inflightID = msgID
	o.sentAt = now
}

// Acked registra el acuse de la foto seq del deploy id. Suelta esa foto y
// cualquier otra más vieja del mismo deploy.
func (o *Outbox) Acked(id uuid.UUID, seq int) {
	o.mu.Lock()
	if o.inflight != nil && o.inflight.DeployID == id && o.inflight.Seq <= seq {
		o.inflight = nil
	}
	if ev, ok := o.pending[id]; ok && ev.Seq <= seq {
		delete(o.pending, id)
	}
	more := len(o.pending) > 0
	onAck := o.onAck
	o.mu.Unlock()
	if onAck != nil {
		onAck(id, seq)
	}
	if more {
		o.signal()
	}
}

// Rejected registra que el control plane rechazó el mensaje msgID. Devuelve si
// era el de la foto en vuelo. Si no es reintentable, la foto se descarta: el
// control plane no la va a aceptar nunca.
func (o *Outbox) Rejected(msgID uuid.UUID, retryable bool) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.inflight == nil || o.inflightID != msgID {
		return false
	}
	if !retryable {
		if ev, ok := o.pending[o.inflight.DeployID]; ok && ev.Seq <= o.inflight.Seq {
			delete(o.pending, o.inflight.DeployID)
		}
	}
	o.inflight = nil
	return true
}

// Restart se llama al abrir un canal nuevo: el acuse de lo que estaba en
// vuelo no va a llegar, así que vuelve a salir.
func (o *Outbox) Restart() {
	o.mu.Lock()
	o.inflight = nil
	more := len(o.pending) > 0
	o.mu.Unlock()
	if more {
		o.signal()
	}
}

// Wake avisa cuando puede haber algo para mandar.
func (o *Outbox) Wake() <-chan struct{} {
	return o.wake
}

// Len dice cuántos deploys tienen una foto sin confirmar.
func (o *Outbox) Len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.pending)
}

func (o *Outbox) signal() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}
