package proto_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

func validEnroll() proto.Enroll {
	return proto.Enroll{
		Token:        "tok_abcdefghijklmnopqrstuvwxyz012345",
		Hostname:     "vps01.example.com",
		AgentVersion: "0.1.0",
		OS:           "linux",
		Arch:         "amd64",
	}
}

func TestMarshalAndParseRoundTrip(t *testing.T) {
	original := validEnroll()

	raw, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("serializar: %v", err)
	}

	envelope, err := proto.ParseEnvelope(raw)
	if err != nil {
		t.Fatalf("leer el envelope: %v", err)
	}
	if envelope.V != proto.Version {
		t.Errorf("versión %d, se esperaba %d", envelope.V, proto.Version)
	}
	if envelope.Type != proto.TypeEnroll {
		t.Errorf("tipo %q, se esperaba %q", envelope.Type, proto.TypeEnroll)
	}
	if envelope.ID == uuid.Nil {
		t.Error("el envelope salió sin ID")
	}
	if envelope.TS.IsZero() {
		t.Error("el envelope salió sin marca de tiempo")
	}
	if envelope.TS.Location() != time.UTC {
		t.Errorf("la marca de tiempo no está en UTC: %v", envelope.TS.Location())
	}

	decoded, err := proto.Decode[proto.Enroll](envelope)
	if err != nil {
		t.Fatalf("decodificar: %v", err)
	}
	if decoded != original {
		t.Errorf("el mensaje cambió al ir y volver:\n  antes: %+v\n  después: %+v", original, decoded)
	}
}

func TestEncodeRejectsInvalidMessage(t *testing.T) {
	invalid := validEnroll()
	invalid.Token = ""

	if _, err := proto.Encode(invalid); err == nil {
		t.Fatal("se codificó un mensaje sin token")
	}
}

func TestParseEnvelopeRejects(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want error
	}{
		{
			name: "versión futura",
			raw:  `{"v":99,"type":"enroll","id":"01924f9c-0000-7000-8000-000000000001","ts":"2026-09-20T12:00:00Z","data":{}}`,
			want: proto.ErrUnsupportedVersion,
		},
		{
			name: "versión cero",
			raw:  `{"type":"enroll","id":"01924f9c-0000-7000-8000-000000000001","ts":"2026-09-20T12:00:00Z","data":{}}`,
			want: proto.ErrUnsupportedVersion,
		},
		{
			name: "tipo desconocido",
			raw:  `{"v":1,"type":"run_shell","id":"01924f9c-0000-7000-8000-000000000001","ts":"2026-09-20T12:00:00Z","data":{}}`,
			want: proto.ErrUnknownType,
		},
		{
			name: "sin datos",
			raw:  `{"v":1,"type":"heartbeat","id":"01924f9c-0000-7000-8000-000000000001","ts":"2026-09-20T12:00:00Z"}`,
			want: proto.ErrEmptyData,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := proto.ParseEnvelope([]byte(tt.raw))
			if !errors.Is(err, tt.want) {
				t.Fatalf("se esperaba %v, se obtuvo %v", tt.want, err)
			}
		})
	}
}

func TestParseEnvelopeRejectsGarbage(t *testing.T) {
	if _, err := proto.ParseEnvelope([]byte("esto no es JSON")); err == nil {
		t.Fatal("se aceptó un mensaje que no es JSON")
	}
}

// Un agent viejo tiene que poder hablar con un control plane que agregó campos.
func TestUnknownFieldsAreIgnored(t *testing.T) {
	raw := `{
		"v":1,
		"type":"heartbeat",
		"id":"01924f9c-0000-7000-8000-000000000001",
		"ts":"2026-09-20T12:00:00Z",
		"campo_nuevo_del_servidor":"lo que sea",
		"data":{"agent_version":"0.1.0","uptime_seconds":42,"metrica_nueva":123}
	}`

	envelope, err := proto.ParseEnvelope([]byte(raw))
	if err != nil {
		t.Fatalf("leer el envelope con campos de más: %v", err)
	}

	heartbeat, err := proto.Decode[proto.Heartbeat](envelope)
	if err != nil {
		t.Fatalf("decodificar con campos de más: %v", err)
	}
	if heartbeat.AgentVersion != "0.1.0" || heartbeat.UptimeSeconds != 42 {
		t.Errorf("los campos conocidos no se leyeron bien: %+v", heartbeat)
	}
}

func TestDecodeRejectsTypeMismatch(t *testing.T) {
	raw, err := proto.Marshal(validEnroll())
	if err != nil {
		t.Fatalf("serializar: %v", err)
	}
	envelope, err := proto.ParseEnvelope(raw)
	if err != nil {
		t.Fatalf("leer el envelope: %v", err)
	}

	if _, err := proto.Decode[proto.Heartbeat](envelope); !errors.Is(err, proto.ErrTypeMismatch) {
		t.Fatalf("se esperaba ErrTypeMismatch, se obtuvo %v", err)
	}
}

func TestDecodeRejectsInvalidPayload(t *testing.T) {
	envelope := proto.Envelope{
		V:    proto.Version,
		Type: proto.TypeHeartbeat,
		ID:   uuid.MustParse("01924f9c-0000-7000-8000-000000000001"),
		TS:   time.Now().UTC(),
		Data: json.RawMessage(`{"agent_version":""}`),
	}

	_, err := proto.Decode[proto.Heartbeat](envelope)
	if err == nil {
		t.Fatal("se decodificó un latido sin versión de agent")
	}
	if !strings.Contains(err.Error(), "versión del agent") {
		t.Errorf("el error no explica el problema: %v", err)
	}
}
