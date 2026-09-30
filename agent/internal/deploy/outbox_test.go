package deploy

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

func event(id uuid.UUID, seq int, started time.Time) proto.DeployEvent {
	return proto.DeployEvent{DeployID: id, Seq: seq, Project: "x", Kind: proto.DeployKindDeploy, Status: proto.DeployRunning, StartedAt: started}
}

func TestOutboxKeepsTheLatestOfEachDeploy(t *testing.T) {
	o := NewOutbox()
	now := time.Now()
	a, b := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())

	o.Put(event(a, 1, now))
	o.Put(event(a, 3, now))
	o.Put(event(a, 2, now)) // más vieja: no reemplaza
	o.Put(event(b, 1, now.Add(time.Minute)))

	ev, ok := o.Next(now)
	if !ok || ev.DeployID != a || ev.Seq != 3 {
		t.Fatalf("Next = %+v, %v; se esperaba la 3 de a (el deploy más viejo)", ev, ok)
	}
	msg := uuid.Must(uuid.NewV7())
	o.Sent(msg, ev, now)

	// Una en vuelo a la vez.
	if _, ok := o.Next(now); ok {
		t.Fatal("salió otra con una en vuelo")
	}
	// Llega una más nueva de a mientras la 3 está en vuelo.
	o.Put(event(a, 4, now))
	o.Acked(a, 3)
	ev, _ = o.Next(now)
	if ev.DeployID != a || ev.Seq != 4 {
		t.Fatalf("después del acuse de la 3 sale %+v, se esperaba la 4 de a", ev)
	}
	o.Sent(uuid.Must(uuid.NewV7()), ev, now)
	o.Acked(a, 4)
	ev, _ = o.Next(now)
	if ev.DeployID != b {
		t.Fatalf("sale %+v, se esperaba b", ev)
	}
}

func TestOutboxResendsAfterSilenceAndRestart(t *testing.T) {
	o := NewOutbox()
	now := time.Now()
	id := uuid.Must(uuid.NewV7())
	o.Put(event(id, 1, now))
	ev, _ := o.Next(now)
	o.Sent(uuid.Must(uuid.NewV7()), ev, now)

	if _, ok := o.Next(now.Add(AckTimeout - time.Second)); ok {
		t.Fatal("salió de nuevo antes del plazo")
	}
	if _, ok := o.Next(now.Add(AckTimeout)); !ok {
		t.Fatal("no salió de nuevo después del plazo")
	}
	o.Sent(uuid.Must(uuid.NewV7()), ev, now)
	o.Restart()
	if _, ok := o.Next(now); !ok {
		t.Fatal("no salió de nuevo al abrir otro canal")
	}
}

func TestOutboxRejections(t *testing.T) {
	o := NewOutbox()
	now := time.Now()
	id := uuid.Must(uuid.NewV7())
	o.Put(event(id, 1, now))
	ev, _ := o.Next(now)
	msg := uuid.Must(uuid.NewV7())
	o.Sent(msg, ev, now)

	if o.Rejected(uuid.Must(uuid.NewV7()), false) {
		t.Fatal("reconoció un rechazo de otro mensaje")
	}
	if !o.Rejected(msg, true) || o.Len() != 1 {
		t.Fatal("un rechazo reintentable tiene que dejarla para volver a salir")
	}
	ev, _ = o.Next(now)
	o.Sent(msg, ev, now)
	if !o.Rejected(msg, false) || o.Len() != 0 {
		t.Fatal("un rechazo definitivo tiene que descartarla")
	}
}

func TestOutboxCallsOnAck(t *testing.T) {
	o := NewOutbox()
	var got []int
	o.setOnAck(func(_ uuid.UUID, seq int) { got = append(got, seq) })
	o.Acked(uuid.Must(uuid.NewV7()), 7)
	if len(got) != 1 || got[0] != 7 {
		t.Fatalf("onAck = %v", got)
	}
}
