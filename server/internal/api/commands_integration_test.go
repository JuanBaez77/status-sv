//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/JuanBaez77/status-sv/proto"
)

type logsBody struct {
	Status    string          `json:"status"`
	Code      string          `json:"code"`
	Message   string          `json:"message"`
	Lines     []proto.LogLine `json:"lines"`
	Truncated bool            `json:"truncated"`
}

// answerCommands hace de agent: lee cada comando que le llega y lo contesta
// con reply. Devuelve los comandos que vio.
func answerCommands(t *testing.T, conn *websocket.Conn, reply func(proto.Envelope, proto.Command) proto.CommandAck) <-chan proto.Command {
	t.Helper()
	seen := make(chan proto.Command, 4)
	go func() {
		for {
			_, raw, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			envelope, err := proto.ParseEnvelope(raw)
			if err != nil || envelope.Type != proto.TypeCommand {
				continue
			}
			cmd, err := proto.Decode[proto.Command](envelope)
			if err != nil {
				continue
			}
			seen <- cmd
			ack, err := proto.Marshal(reply(envelope, cmd))
			if err != nil {
				return
			}
			_ = conn.Write(context.Background(), websocket.MessageText, ack)
		}
	}()
	return seen
}

func (f *fixture) postLogs(t *testing.T, serverID, body string, cookie *http.Cookie) (int, logsBody) {
	t.Helper()
	resp := f.do(t, http.MethodPost, "/v1/servers/"+serverID+"/logs", body, "198.51.100.80", cookie,
		map[string]string{"Content-Type": "application/json"})
	var out logsBody
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(resp.body, &out); err != nil {
			t.Fatalf("leer la respuesta: %v", err)
		}
	}
	return resp.StatusCode, out
}

func TestLogsGoToTheAgentAndBack(t *testing.T) {
	f := setup(t)
	cookie := f.login(t, "198.51.100.80")
	agent, serverID := connectAgent(t, f, "vps-logs", "203.0.113.80")

	at := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	seen := answerCommands(t, agent, func(envelope proto.Envelope, _ proto.Command) proto.CommandAck {
		return proto.CommandAck{CommandID: envelope.ID, Status: proto.CommandOK, Logs: &proto.LogsResult{
			Lines: []proto.LogLine{{TS: at, Stream: "stderr", Text: "conexión rechazada"}},
		}}
	})

	status, body := f.postLogs(t, serverID.String(), `{"kind":"service","target":"nginx.service","lines":50}`, cookie)
	if status != http.StatusOK || body.Status != "ok" || len(body.Lines) != 1 || body.Lines[0].Text != "conexión rechazada" {
		t.Fatalf("respuesta: %d %+v", status, body)
	}

	cmd := <-seen
	if cmd.Operation != proto.OpGetLogs || cmd.GetLogs.Target != "nginx.service" || cmd.GetLogs.Lines != 50 {
		t.Errorf("el agent recibió: %+v", cmd.GetLogs)
	}
}

// Un comando que el agent rechaza no es un error de HTTP: el panel muestra el motivo.
func TestLogsRejectedByTheAgent(t *testing.T) {
	f := setup(t)
	cookie := f.login(t, "198.51.100.81")
	agent, serverID := connectAgent(t, f, "vps-logs-2", "203.0.113.81")
	answerCommands(t, agent, func(envelope proto.Envelope, _ proto.Command) proto.CommandAck {
		return proto.CommandAck{CommandID: envelope.ID, Status: proto.CommandRejected, Code: proto.CommandUnknownTarget, Message: "el agent no ve eso"}
	})

	status, body := f.postLogs(t, serverID.String(), `{"kind":"container","target":"`+strings.Repeat("ab", 32)+`","lines":10}`, cookie)
	if status != http.StatusOK || body.Status != "rejected" || body.Code != "unknown_target" || body.Lines == nil {
		t.Fatalf("respuesta: %d %+v", status, body)
	}
}

func TestLogsRequestErrors(t *testing.T) {
	f := setup(t)
	cookie := f.login(t, "198.51.100.82")
	ack := f.enrollOK(t, "vps-sin-agent", "203.0.113.82")
	id := ack.ServerID.String()

	tests := []struct {
		name   string
		body   string
		cookie *http.Cookie
		want   int
	}{
		{"sin sesión", `{"kind":"service","target":"nginx.service","lines":10}`, nil, http.StatusUnauthorized},
		{"agent desconectado", `{"kind":"service","target":"nginx.service","lines":10}`, cookie, http.StatusConflict},
		{"objetivo inválido", `{"kind":"service","target":"; rm -rf /","lines":10}`, cookie, http.StatusBadRequest},
		{"tipo inventado", `{"kind":"archivo","target":"/etc/shadow","lines":10}`, cookie, http.StatusBadRequest},
		{"demasiados renglones", `{"kind":"service","target":"nginx.service","lines":5000}`, cookie, http.StatusBadRequest},
		{"campo de más", `{"kind":"service","target":"nginx.service","lines":10,"since":"ayer"}`, cookie, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if status, _ := f.postLogs(t, id, tt.body, tt.cookie); status != tt.want {
				t.Errorf("estado %d, se esperaba %d", status, tt.want)
			}
		})
	}
}

// Un agent no puede contestar un comando que se le mandó a otro.
func TestAgentCannotAnswerForAnother(t *testing.T) {
	f := setup(t)
	cookie := f.login(t, "198.51.100.83")
	target, targetID := connectAgent(t, f, "vps-objetivo", "203.0.113.83")
	intruder, _ := connectAgent(t, f, "vps-intruso", "203.0.113.84")

	// El objetivo recibe el comando pero no contesta; el intruso contesta con
	// el ID de ese comando por su propio canal.
	ids := make(chan string, 1)
	go func() {
		for {
			_, raw, err := target.Read(context.Background())
			if err != nil {
				return
			}
			if envelope, err := proto.ParseEnvelope(raw); err == nil && envelope.Type == proto.TypeCommand {
				ids <- envelope.ID.String()
				ack, _ := proto.Marshal(proto.CommandAck{CommandID: envelope.ID, Status: proto.CommandOK,
					Logs: &proto.LogsResult{Lines: []proto.LogLine{{Text: "falso"}}}})
				_ = intruder.Write(context.Background(), websocket.MessageText, ack)
			}
		}
	}()

	done := make(chan int, 1)
	go func() {
		status, _ := f.postLogs(t, targetID.String(), `{"kind":"service","target":"nginx.service","lines":10}`, cookie)
		done <- status
	}()

	<-ids
	// La respuesta del intruso no vale: el pedido sigue esperando al agent de
	// verdad, que nunca contesta, hasta que se vence el plazo.
	select {
	case status := <-done:
		if status != http.StatusGatewayTimeout {
			t.Fatalf("estado %d, se esperaba %d (sin respuesta del agent de verdad)", status, http.StatusGatewayTimeout)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("el pedido no terminó")
	}
}
