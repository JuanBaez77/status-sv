package proto_test

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

// -update regenera los archivos de proto/testdata. Se usa cuando el contrato
// cambia a propósito, y el diff del PR muestra exactamente qué se movió en el cable.
var update = flag.Bool("update", false, "regenera los archivos de testdata")

// Valores fijos para que el archivo generado no cambie entre corridas.
var (
	goldenID       = uuid.MustParse("01924f9c-0000-7000-8000-000000000001")
	goldenServerID = uuid.MustParse("01924f9c-0000-7000-8000-0000000000aa")
	goldenTS       = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
)

// TestGoldenEnvelopes fija el formato exacto que viaja por el WebSocket. Si un
// cambio de código mueve un nombre de campo, este test lo muestra como un diff
// del archivo, que es justamente lo que hay que revisar con cuidado: del otro
// lado puede haber un agent viejo.
func TestGoldenEnvelopes(t *testing.T) {
	tests := []struct {
		name string
		msg  proto.Message
	}{
		{name: "enroll", msg: validEnroll()},
		{
			name: "enroll_ack",
			msg: proto.EnrollAck{
				ServerID:   goldenServerID,
				ServerName: "vps-01",
				Credential: "cred_abcdefghijklmnopqrstuvwxyz012345",
				Config:     proto.DefaultAgentConfig(),
			},
		},
		{
			name: "heartbeat",
			msg: proto.Heartbeat{
				AgentVersion:    "0.1.0",
				UptimeSeconds:   3600,
				SelfMemoryBytes: 22 << 20,
				SelfCPUPercent:  0.4,
			},
		},
		{name: "heartbeat_ack", msg: proto.HeartbeatAck{ServerTime: goldenTS}},
		{name: "metrics", msg: proto.Metrics{Samples: []proto.Sample{validSample()}}},
		{name: "metrics_ack", msg: proto.MetricsAck{Accepted: 1, LastTS: goldenTS}},
		{name: "runtime_state", msg: validRuntimeState()},
		{name: "runtime_state_ack", msg: proto.RuntimeStateAck{TakenAt: goldenTS}},
		{name: "error", msg: proto.NewError(proto.CodeInvalidToken, "el token no existe o ya se usó", goldenID)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.msg)
			if err != nil {
				t.Fatalf("serializar el mensaje: %v", err)
			}

			envelope := proto.Envelope{
				V:    proto.Version,
				Type: tt.msg.Type(),
				ID:   goldenID,
				TS:   goldenTS,
				Data: data,
			}

			got, err := json.MarshalIndent(envelope, "", "  ")
			if err != nil {
				t.Fatalf("serializar el envelope: %v", err)
			}
			got = append(got, '\n')

			path := filepath.Join("testdata", tt.name+".json")
			if *update {
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatalf("escribir %s: %v", path, err)
				}
				return
			}

			want, err := os.ReadFile(path) //nolint:gosec // ruta fija de testdata
			if err != nil {
				t.Fatalf("leer %s (¿falta correr los tests con -update?): %v", path, err)
			}

			// Normaliza los fines de línea, porque en Windows el checkout puede traerlos con CR.
			if normalize(string(got)) != normalize(string(want)) {
				t.Errorf("el formato del mensaje %q cambió.\n--- esperado (%s) ---\n%s\n--- obtenido ---\n%s",
					tt.name, path, want, got)
			}

			// Lo que quedó fijado tiene que poder leerse de vuelta.
			parsed, err := proto.ParseEnvelope(want)
			if err != nil {
				t.Fatalf("el archivo fijado no se puede leer: %v", err)
			}
			if parsed.Type != tt.msg.Type() {
				t.Errorf("el archivo fijado tiene tipo %q y se esperaba %q", parsed.Type, tt.msg.Type())
			}
		})
	}
}

func normalize(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// Todo tipo de mensaje que existe en el código tiene que estar en la lista de
// tipos conocidos del envelope: si no, el receptor lo rechazaría.
func TestEveryMessageTypeIsAccepted(t *testing.T) {
	messages := []proto.Message{
		proto.Enroll{},
		proto.EnrollAck{},
		proto.Heartbeat{},
		proto.HeartbeatAck{},
		proto.Metrics{},
		proto.MetricsAck{},
		proto.RuntimeState{},
		proto.RuntimeStateAck{},
		proto.ErrorMessage{},
	}

	for _, msg := range messages {
		raw, err := json.Marshal(proto.Envelope{
			V:    proto.Version,
			Type: msg.Type(),
			ID:   goldenID,
			TS:   goldenTS,
			Data: json.RawMessage(`{}`),
		})
		if err != nil {
			t.Fatalf("serializar el envelope de %q: %v", msg.Type(), err)
		}
		if _, err := proto.ParseEnvelope(raw); errors.Is(err, proto.ErrUnknownType) {
			t.Errorf("el tipo %q no está en la lista de tipos conocidos", msg.Type())
		}
	}
}
