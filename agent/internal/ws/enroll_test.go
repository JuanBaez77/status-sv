package ws

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

func TestEnrollHappyPath(t *testing.T) {
	serverID := uuid.MustParse("01924f9c-0000-7000-8000-0000000000aa")

	var received proto.Enroll
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/agents/enroll" {
			t.Errorf("el agent pidió %s %s", r.Method, r.URL.Path)
		}

		var envelope proto.Envelope
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Fatalf("leer el pedido: %v", err)
		}
		message, err := proto.Decode[proto.Enroll](envelope)
		if err != nil {
			t.Fatalf("decodificar el pedido: %v", err)
		}
		received = message

		raw, err := proto.Marshal(proto.EnrollAck{
			ServerID:   serverID,
			ServerName: "vps-01",
			Credential: "svagt_credencial-de-prueba-suficientemente-larga",
			Config:     proto.DefaultAgentConfig(),
		})
		if err != nil {
			t.Fatalf("armar la respuesta: %v", err)
		}
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	got, err := Enroll(t.Context(), srv.URL, "svenr_token-de-prueba-suficientemente-largo", "vps01.example.com", "0.1.0", "linux", "amd64")
	if err != nil {
		t.Fatalf("enrolar: %v", err)
	}

	if received.Token != "svenr_token-de-prueba-suficientemente-largo" {
		t.Errorf("el servidor recibió el token %q", received.Token)
	}
	if received.Hostname != "vps01.example.com" || received.OS != "linux" || received.Arch != "amd64" {
		t.Errorf("el pedido llegó incompleto: %+v", received)
	}

	if got.ServerID != serverID || got.ServerName != "vps-01" {
		t.Errorf("el estado devuelto no coincide: %+v", got)
	}
	if got.ServerURL != srv.URL {
		t.Errorf("ServerURL = %q, se esperaba %q", got.ServerURL, srv.URL)
	}
	if got.EnrolledAt.IsZero() {
		t.Error("el estado quedó sin fecha de enrolamiento")
	}
	if err := got.Validate(); err != nil {
		t.Errorf("el estado devuelto no es válido: %v", err)
	}
}

func TestEnrollReportsRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		raw, err := proto.Marshal(proto.NewError(proto.CodeInvalidToken, "el token no existe, ya se usó o venció", uuid.Nil))
		if err != nil {
			t.Fatalf("armar el rechazo: %v", err)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	_, err := Enroll(t.Context(), srv.URL, "svenr_token-que-no-sirve-pero-es-largo", "vps01", "0.1.0", "linux", "amd64")

	var rejected *ErrEnrollRejected
	if !errors.As(err, &rejected) {
		t.Fatalf("se esperaba ErrEnrollRejected, se obtuvo %v", err)
	}
	if rejected.Code != proto.CodeInvalidToken {
		t.Errorf("código %q", rejected.Code)
	}
	// Reintentar con el mismo token no tiene sentido: el agent no debe insistir.
	if rejected.Retryable() {
		t.Error("un token inválido no debería marcarse como reintentable")
	}
}

func TestEnrollRejectsUnreadableResponses(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{
			name:    "no es un envelope",
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hola")) },
			want:    "no se entiende",
		},
		{
			name: "versión de protocolo futura",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"v":99,"type":"enroll_ack","id":"01924f9c-0000-7000-8000-000000000001","ts":"2026-09-20T12:00:00Z","data":{}}`))
			},
			want: "no se entiende",
		},
		{
			name: "enroll_ack sin credencial",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"v":1,"type":"enroll_ack","id":"01924f9c-0000-7000-8000-000000000001","ts":"2026-09-20T12:00:00Z","data":{"server_name":"vps-01"}}`))
			},
			want: "leer la respuesta de enrolamiento",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			_, err := Enroll(t.Context(), srv.URL, "svenr_token-de-prueba-suficientemente-largo", "vps01", "0.1.0", "linux", "amd64")
			if err == nil {
				t.Fatal("se aceptó una respuesta que no sirve")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("el error no menciona %q: %v", tt.want, err)
			}
		})
	}
}

func TestEnrollRejectsBadServerURL(t *testing.T) {
	urls := []string{"", "   ", "ftp://control", "no-es-una-url", "https://"}

	for _, raw := range urls {
		t.Run(raw, func(t *testing.T) {
			if _, err := Enroll(t.Context(), raw, "svenr_token-de-prueba-suficientemente-largo", "vps01", "0.1.0", "linux", "amd64"); err == nil {
				t.Errorf("se aceptó la dirección %q", raw)
			}
		})
	}
}

func TestWebsocketURL(t *testing.T) {
	tests := []struct {
		serverURL string
		want      string
	}{
		{serverURL: "https://status.example.com", want: "wss://status.example.com/v1/agents/connect"},
		{serverURL: "https://status.example.com/", want: "wss://status.example.com/v1/agents/connect"},
		{serverURL: "http://127.0.0.1:8080", want: "ws://127.0.0.1:8080/v1/agents/connect"},
		{serverURL: "https://ejemplo.com/status-sv", want: "wss://ejemplo.com/status-sv/v1/agents/connect"},
	}

	for _, tt := range tests {
		t.Run(tt.serverURL, func(t *testing.T) {
			got, err := websocketURL(tt.serverURL)
			if err != nil {
				t.Fatalf("convertir la URL: %v", err)
			}
			if got != tt.want {
				t.Errorf("websocketURL(%q) = %q, se esperaba %q", tt.serverURL, got, tt.want)
			}
		})
	}
}
