package ws

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/JuanBaez77/status-sv/agent/internal/runtimestate"
	"github.com/JuanBaez77/status-sv/proto"
)

func (f *fakeControlPlane) handleRuntime(ctx context.Context, conn *websocket.Conn, envelope proto.Envelope) error {
	state, err := proto.Decode[proto.RuntimeState](envelope)
	if err != nil {
		return err
	}

	f.mu.Lock()
	f.runtimeStates = append(f.runtimeStates, state)
	mode := f.runtimeMode
	f.mu.Unlock()

	var reply proto.Message
	switch mode {
	case metricsAck:
		reply = proto.RuntimeStateAck{TakenAt: state.TakenAt}
	case metricsReject:
		reply = proto.NewError(proto.CodeInvalidMessage, "este control plane todavía no guarda fotos", envelope.ID)
	case metricsSilent:
		return nil
	}
	raw, err := proto.Marshal(reply)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, raw)
}

func (f *fakeControlPlane) runtimeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runtimeStates)
}

func runtimeSnap(at time.Time) proto.RuntimeState {
	ok := proto.RuntimeSource{Status: proto.SourceOK}
	return proto.RuntimeState{TakenAt: at, Sources: proto.RuntimeSources{Docker: ok, Systemd: ok, Processes: ok}}
}

func TestClientSendsRuntimeStateAndReleasesItOnAck(t *testing.T) {
	fake := newFakeControlPlane(t)
	client := newTestClient(t, fake)
	outbox := runtimestate.NewOutbox()
	client.SetRuntime(outbox)
	runClient(t, client)

	at := time.Now().UTC().Truncate(time.Millisecond)
	outbox.Put(runtimeSnap(at))
	eventually(t, "que llegue la foto", func() bool { return fake.runtimeCount() == 1 })

	// Con el acuse, la bandeja queda libre y la foto siguiente sale enseguida.
	outbox.Put(runtimeSnap(at.Add(time.Second)))
	eventually(t, "que llegue la segunda foto", func() bool { return fake.runtimeCount() == 2 })
}

func TestClientDropsRuntimeStateTheControlPlaneRejects(t *testing.T) {
	fake := newFakeControlPlane(t)
	fake.runtimeMode = metricsReject
	client := newTestClient(t, fake)
	outbox := runtimestate.NewOutbox()
	client.SetRuntime(outbox)
	runClient(t, client)

	outbox.Put(runtimeSnap(time.Now().UTC()))
	eventually(t, "que llegue la foto", func() bool { return fake.runtimeCount() == 1 })
	eventually(t, "que la bandeja descarte la foto rechazada", func() bool {
		_, pending := outbox.Next(time.Now())
		return !pending
	})
}

// Si el canal se corta con una foto sin confirmar, sale de nuevo en el canal
// siguiente.
func TestClientResendsRuntimeStateAfterReconnecting(t *testing.T) {
	fake := newFakeControlPlane(t)
	fake.runtimeMode = metricsSilent
	client := newTestClient(t, fake)
	outbox := runtimestate.NewOutbox()
	client.SetRuntime(outbox)
	runClient(t, client)

	outbox.Put(runtimeSnap(time.Now().UTC()))
	eventually(t, "que llegue la foto", func() bool { return fake.runtimeCount() == 1 })

	fake.mu.Lock()
	fake.runtimeMode = metricsAck
	fake.mu.Unlock()
	fake.dropConnections()
	eventually(t, "que la foto vuelva a salir después de reconectar", func() bool { return fake.runtimeCount() >= 2 })
}
