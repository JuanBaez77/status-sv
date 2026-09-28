package ws

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

// fakeHandler contesta todo comando con unos logs fijos.
type fakeHandler struct{ seen chan uuid.UUID }

func (h *fakeHandler) Handle(_ context.Context, id uuid.UUID, _ proto.Command) proto.CommandAck {
	h.seen <- id
	return proto.CommandAck{CommandID: id, Status: proto.CommandOK, Logs: &proto.LogsResult{
		Lines: []proto.LogLine{{Text: "hola"}},
	}}
}

// sendCommand manda un comando crudo por el primer canal abierto y espera la
// respuesta del agent.
func sendCommand(t *testing.T, fake *fakeControlPlane, raw []byte) proto.CommandAck {
	t.Helper()
	eventually(t, "que el agent se conecte", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.conns) > 0
	})
	fake.mu.Lock()
	conn := fake.conns[len(fake.conns)-1]
	fake.acks = make(chan proto.CommandAck, 1)
	acks := fake.acks
	fake.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("mandar el comando: %v", err)
	}
	select {
	case ack := <-acks:
		return ack
	case <-ctx.Done():
		t.Fatal("no llegó la respuesta al comando")
		return proto.CommandAck{}
	}
}

func TestClientRunsCommands(t *testing.T) {
	fake := newFakeControlPlane(t)
	client := newTestClient(t, fake)
	handler := &fakeHandler{seen: make(chan uuid.UUID, 1)}
	client.SetCommands(handler)
	runClient(t, client)

	raw, err := proto.Marshal(proto.Command{Operation: proto.OpGetLogs, GetLogs: &proto.GetLogsParams{
		Kind: proto.LogService, Target: "nginx.service", Lines: 10,
	}})
	if err != nil {
		t.Fatalf("armar el comando: %v", err)
	}
	var envelope proto.Envelope
	_ = json.Unmarshal(raw, &envelope)

	ack := sendCommand(t, fake, raw)
	if ack.CommandID != envelope.ID || ack.Status != proto.CommandOK || len(ack.Logs.Lines) != 1 {
		t.Errorf("respuesta: %+v", ack)
	}
	if got := <-handler.seen; got != envelope.ID {
		t.Errorf("el ejecutor recibió el comando %s", got)
	}
}

// Una operación que no está en la lista cerrada se rechaza sin llegar al
// ejecutor, aunque venga bien armada.
func TestClientRejectsUnknownOperations(t *testing.T) {
	fake := newFakeControlPlane(t)
	client := newTestClient(t, fake)
	handler := &fakeHandler{seen: make(chan uuid.UUID, 1)}
	client.SetCommands(handler)
	runClient(t, client)

	id := uuid.New()
	raw, _ := json.Marshal(proto.Envelope{
		V: proto.Version, Type: proto.TypeCommand, ID: id, TS: time.Now().UTC(),
		Data: json.RawMessage(`{"operation":"run_shell","args":"rm -rf /"}`),
	})
	ack := sendCommand(t, fake, raw)
	if ack.CommandID != id || ack.Status != proto.CommandRejected || ack.Code != proto.CommandUnknownOperation {
		t.Errorf("respuesta: %+v", ack)
	}
	select {
	case <-handler.seen:
		t.Error("el ejecutor no tendría que haberlo visto")
	default:
	}
}

// Sin ejecutor (la prueba de carga, por ejemplo), todo comando se rechaza.
func TestClientWithoutExecutorRejects(t *testing.T) {
	fake := newFakeControlPlane(t)
	client := newTestClient(t, fake)
	runClient(t, client)

	raw, _ := proto.Marshal(proto.Command{Operation: proto.OpGetLogs, GetLogs: &proto.GetLogsParams{
		Kind: proto.LogService, Target: "nginx.service", Lines: 10,
	}})
	if ack := sendCommand(t, fake, raw); ack.Status != proto.CommandRejected {
		t.Errorf("respuesta: %+v", ack)
	}
}
