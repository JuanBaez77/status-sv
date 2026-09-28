//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

// runtimeBody es la respuesta de GET /v1/servers/{id}/runtime, tal como la ve
// el front.
type runtimeBody struct {
	ServerID uuid.UUID  `json:"server_id"`
	TakenAt  *time.Time `json:"taken_at"`
	Sources  map[string]struct {
		Status  string `json:"status"`
		Detail  string `json:"detail"`
		Omitted int    `json:"omitted"`
	} `json:"sources"`
	Containers []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		State string `json:"state"`
		Usage *struct {
			CPUPercent  float64 `json:"cpu_percent"`
			MemoryBytes int64   `json:"memory_bytes"`
		} `json:"usage"`
	} `json:"containers"`
	Services []struct {
		Unit        string `json:"unit"`
		ActiveState string `json:"active_state"`
	} `json:"services"`
	Processes []json.RawMessage `json:"processes"`
}

// connectAgent enrola un agent y abre su canal.
func connectAgent(t *testing.T, f *fixture, name, ip string) (*websocket.Conn, uuid.UUID) {
	t.Helper()
	ack := f.enrollOK(t, name, ip)
	conn, _, err := f.dial(t, ack.Credential)
	if err != nil {
		t.Fatalf("abrir el canal: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return conn, ack.ServerID
}

// sendRuntime manda una foto y espera su acuse.
func sendRuntime(t *testing.T, conn *websocket.Conn, state proto.RuntimeState) {
	t.Helper()
	sendEnvelope(t, conn, state)
	if envelope := receive(t, conn); envelope.Type != proto.TypeRuntimeStateAck {
		t.Fatalf("llegó %q, se esperaba runtime_state_ack", envelope.Type)
	}
}

func (f *fixture) getRuntime(t *testing.T, serverID string, cookie *http.Cookie) (int, runtimeBody) {
	t.Helper()
	resp := f.do(t, http.MethodGet, fmt.Sprintf("/v1/servers/%s/runtime", serverID), "", "198.51.100.70", cookie, nil)
	var body runtimeBody
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(resp.body, &body); err != nil {
			t.Fatalf("leer la respuesta: %v", err)
		}
	}
	return resp.StatusCode, body
}

func TestServerRuntimeAPI(t *testing.T) {
	f := setup(t)
	cookie := f.login(t, "198.51.100.70")
	agent, serverID := connectAgent(t, f, "vps-runtime", "203.0.113.70")

	// Antes de la primera foto: sin marca y con las listas vacías, no null.
	status, body := f.getRuntime(t, serverID.String(), cookie)
	if status != http.StatusOK || body.TakenAt != nil || body.Containers == nil || len(body.Containers) != 0 {
		t.Fatalf("antes de la primera foto: %d %+v", status, body)
	}

	at := time.Now().UTC().Truncate(time.Millisecond)
	state := runtimeState(at)
	state.Containers[0].Usage = &proto.Usage{CPUPercent: 12.5, MemoryBytes: 64 << 20}
	state.Omitted.Processes = 4
	sendRuntime(t, agent, state)

	status, body = f.getRuntime(t, serverID.String(), cookie)
	if status != http.StatusOK {
		t.Fatalf("estado %d", status)
	}
	if body.TakenAt == nil || !body.TakenAt.Equal(at) {
		t.Errorf("marca: %v, se mandó %v", body.TakenAt, at)
	}
	if len(body.Containers) != 1 || body.Containers[0].Name != "web" || body.Containers[0].State != "running" {
		t.Fatalf("contenedores: %+v", body.Containers)
	}
	if u := body.Containers[0].Usage; u == nil || u.CPUPercent != 12.5 || u.MemoryBytes != 64<<20 {
		t.Errorf("consumo del contenedor: %+v", u)
	}
	if len(body.Services) != 1 || body.Services[0].Unit != "nginx.service" || body.Services[0].ActiveState != "active" {
		t.Errorf("servicios: %+v", body.Services)
	}
	if body.Sources["processes"].Omitted != 4 || body.Sources["docker"].Status != "ok" {
		t.Errorf("fuentes: %+v", body.Sources)
	}
}

func TestServerRuntimeAPIRejects(t *testing.T) {
	f := setup(t)
	cookie := f.login(t, "198.51.100.71")

	if status, _ := f.getRuntime(t, uuid.Must(uuid.NewV7()).String(), cookie); status != http.StatusNotFound {
		t.Errorf("servidor que no existe: %d", status)
	}
	if status, _ := f.getRuntime(t, "no-es-un-uuid", cookie); status != http.StatusBadRequest {
		t.Errorf("ID inválido: %d", status)
	}
	if status, _ := f.getRuntime(t, uuid.Must(uuid.NewV7()).String(), nil); status != http.StatusUnauthorized {
		t.Errorf("sin sesión: %d", status)
	}
}

// El panel suscripto a un servidor recibe la foto apenas se guarda.
func TestPanelReceivesRuntimeLive(t *testing.T) {
	f := setup(t)
	cookie := f.login(t, "198.51.100.72")
	agent, serverID := connectAgent(t, f, "vps-runtime-vivo", "203.0.113.72")

	panelConn, status, err := f.dialPanel(t, cookie)
	if err != nil {
		t.Fatalf("abrir el canal del panel: %v (estado %d)", err, status)
	}
	readPanel(t, panelConn) // bienvenida
	writePanel(t, panelConn, `{"type":"subscribe","server_ids":["`+serverID.String()+`"]}`)
	if ack := readPanel(t, panelConn); ack.Type != "subscribed" {
		t.Fatalf("confirmación = %+v", ack)
	}

	sendRuntime(t, agent, runtimeState(time.Now().UTC().Truncate(time.Millisecond)))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, raw, err := panelConn.Read(ctx)
	if err != nil {
		t.Fatalf("leer del canal del panel: %v", err)
	}
	var message struct {
		Type     string      `json:"type"`
		ServerID uuid.UUID   `json:"server_id"`
		Runtime  runtimeBody `json:"runtime"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		t.Fatalf("leer %s: %v", raw, err)
	}
	if message.Type != "runtime" || message.ServerID != serverID {
		t.Fatalf("mensaje = %s", raw)
	}
	if len(message.Runtime.Containers) != 1 || message.Runtime.Containers[0].Name != "web" {
		t.Errorf("la foto del panel no trae el contenedor: %s", raw)
	}
}
