package runtimestate

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

func snap(at time.Time) proto.RuntimeState {
	return proto.RuntimeState{TakenAt: at, Sources: proto.RuntimeSources{Docker: ok, Systemd: ok, Processes: ok}}
}

func mustNext(t *testing.T, o *Outbox, now time.Time) proto.RuntimeState {
	t.Helper()
	s, ok := o.Next(now)
	if !ok {
		t.Fatal("esperaba una foto para mandar")
	}
	return s
}

func TestOutboxOneInFlight(t *testing.T) {
	o := NewOutbox()
	if _, ok := o.Next(t0); ok {
		t.Fatal("una bandeja vacía no tiene nada")
	}

	o.Put(snap(t0))
	s := mustNext(t, o, t0)
	id := uuid.New()
	o.Sent(id, s.TakenAt, t0)

	// Llega otra foto mientras la primera espera: no sale hasta el acuse.
	o.Put(snap(t0.Add(time.Second)))
	if _, ok := o.Next(t0.Add(time.Second)); ok {
		t.Fatal("con una foto en vuelo no sale otra")
	}

	o.Acked(t0)
	if s := mustNext(t, o, t0.Add(2*time.Second)); !s.TakenAt.Equal(t0.Add(time.Second)) {
		t.Errorf("después del acuse sale la nueva: %v", s.TakenAt)
	}
}

func TestOutboxNewestReplacesPending(t *testing.T) {
	o := NewOutbox()
	o.Put(snap(t0))
	o.Put(snap(t0.Add(time.Second)))
	o.Put(snap(t0.Add(2 * time.Second)))
	if s := mustNext(t, o, t0); !s.TakenAt.Equal(t0.Add(2 * time.Second)) {
		t.Errorf("tiene que salir solo la más nueva: %v", s.TakenAt)
	}
}

func TestOutboxResendsAfterTimeout(t *testing.T) {
	o := NewOutbox()
	o.Put(snap(t0))
	o.Sent(uuid.New(), t0, t0)

	if _, ok := o.Next(t0.Add(AckTimeout - time.Second)); ok {
		t.Fatal("antes del plazo no se reenvía")
	}
	if s := mustNext(t, o, t0.Add(AckTimeout)); !s.TakenAt.Equal(t0) {
		t.Errorf("sin acuse, la misma foto vuelve a salir: %v", s.TakenAt)
	}
}

func TestOutboxRejections(t *testing.T) {
	t.Run("reintentable", func(t *testing.T) {
		o := NewOutbox()
		o.Put(snap(t0))
		id := uuid.New()
		o.Sent(id, t0, t0)
		if !o.Rejected(id, true) {
			t.Fatal("el rechazo era de la foto en vuelo")
		}
		mustNext(t, o, t0)
	})
	t.Run("definitivo", func(t *testing.T) {
		o := NewOutbox()
		o.Put(snap(t0))
		id := uuid.New()
		o.Sent(id, t0, t0)
		o.Rejected(id, false)
		if _, ok := o.Next(t0); ok {
			t.Fatal("una foto rechazada para siempre se descarta")
		}
	})
	t.Run("de otro mensaje", func(t *testing.T) {
		o := NewOutbox()
		o.Put(snap(t0))
		o.Sent(uuid.New(), t0, t0)
		if o.Rejected(uuid.New(), false) {
			t.Fatal("un rechazo de otro mensaje no es de esta bandeja")
		}
	})
}

// Al abrir un canal nuevo, el acuse de la foto en vuelo no va a llegar: vuelve
// a salir, salvo que ya haya una más nueva.
func TestOutboxRestart(t *testing.T) {
	o := NewOutbox()
	o.Put(snap(t0))
	o.Sent(uuid.New(), t0, t0)
	o.Restart()
	if s := mustNext(t, o, t0); !s.TakenAt.Equal(t0) {
		t.Errorf("después de reconectar sale la que estaba en vuelo: %v", s.TakenAt)
	}

	o.Sent(uuid.New(), t0, t0)
	o.Put(snap(t0.Add(time.Second)))
	o.Restart()
	if s := mustNext(t, o, t0); !s.TakenAt.Equal(t0.Add(time.Second)) {
		t.Errorf("si había una más nueva, sale esa: %v", s.TakenAt)
	}
}

// Un acuse que llega tarde, de una foto que ya había vuelto a la espera, la
// saca de la bandeja: el control plane ya la tiene.
func TestOutboxLateAck(t *testing.T) {
	o := NewOutbox()
	o.Put(snap(t0))
	o.Sent(uuid.New(), t0, t0)
	o.Next(t0.Add(AckTimeout)) // vuelve a la espera
	o.Acked(t0)
	if _, ok := o.Next(t0.Add(AckTimeout)); ok {
		t.Fatal("la foto ya confirmada no tiene que volver a salir")
	}
}

func TestOutboxWakes(t *testing.T) {
	o := NewOutbox()
	o.Put(snap(t0))
	select {
	case <-o.Wake():
	default:
		t.Fatal("Put tiene que despertar al que manda")
	}
}
