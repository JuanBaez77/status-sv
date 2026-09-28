//go:build integration

package api_test

import (
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/JuanBaez77/status-sv/proto"
)

func runtimeState(at time.Time) proto.RuntimeState {
	ok := proto.RuntimeSource{Status: proto.SourceOK}
	return proto.RuntimeState{
		TakenAt: at,
		Sources: proto.RuntimeSources{Docker: ok, Systemd: ok, Processes: ok},
		Containers: []proto.Container{{
			ID: strings.Repeat("c1", 32), Name: "web", Image: "nginx:1.27", State: proto.ContainerRunning,
		}},
		Services: []proto.Service{{
			Unit: "nginx.service", LoadState: "loaded", ActiveState: proto.ActiveActive, SubState: "running",
		}},
	}
}

// El camino que hace el agent cuando algo cambia: manda la foto, se guarda y
// llega el acuse con la misma marca.
func TestRuntimeStateIsStoredAndAcknowledged(t *testing.T) {
	f := setup(t)
	ack := f.enrollOK(t, "vps-rt", "203.0.113.60")
	conn, _, err := f.dial(t, ack.Credential)
	if err != nil {
		t.Fatalf("abrir el canal: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	at := time.Now().UTC().Truncate(time.Millisecond)
	for attempt := range 2 {
		// El segundo envío es el reenvío de una foto cuyo acuse se perdió.
		sendEnvelope(t, conn, runtimeState(at))
		envelope := receive(t, conn)
		if envelope.Type != proto.TypeRuntimeStateAck {
			t.Fatalf("envío %d: llegó %q, se esperaba runtime_state_ack", attempt+1, envelope.Type)
		}
		got, err := proto.Decode[proto.RuntimeStateAck](envelope)
		if err != nil {
			t.Fatalf("leer el acuse: %v", err)
		}
		if !got.TakenAt.Equal(at) {
			t.Errorf("el acuse confirma %v, se mandó %v", got.TakenAt, at)
		}
	}

	rows, err := f.queries.ListRuntimeUnits(t.Context(), ack.ServerID)
	if err != nil {
		t.Fatalf("listar unidades: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("esperaba el contenedor y el servicio, hay %d unidades", len(rows))
	}
}

// Una foto con el reloj muy adelantado no se va a aceptar nunca: invalid_message,
// y el agent la descarta.
func TestRuntimeStateFromTheFutureIsRejected(t *testing.T) {
	f := setup(t)
	ack := f.enrollOK(t, "vps-rt2", "203.0.113.61")
	conn, _, err := f.dial(t, ack.Credential)
	if err != nil {
		t.Fatalf("abrir el canal: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	id := sendEnvelope(t, conn, runtimeState(time.Now().UTC().Add(time.Hour)))
	envelope := receive(t, conn)
	if envelope.Type != proto.TypeError {
		t.Fatalf("llegó %q, se esperaba error", envelope.Type)
	}
	msg, err := proto.Decode[proto.ErrorMessage](envelope)
	if err != nil {
		t.Fatalf("leer el error: %v", err)
	}
	if msg.Code != proto.CodeInvalidMessage || msg.InReplyTo == nil || *msg.InReplyTo != id {
		t.Errorf("error inesperado: %+v", msg)
	}
}
