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
	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

// Mensajes del canal del panel, tal como los ve el front.
type panelMessage struct {
	Type                string      `json:"type"`
	Message             string      `json:"message"`
	OfflineAfterSeconds int         `json:"offline_after_seconds"`
	ServerID            uuid.UUID   `json:"server_id"`
	ServerIDs           []uuid.UUID `json:"server_ids"`
	Sample              struct {
		TS             time.Time `json:"ts"`
		CPUUsedPercent float64   `json:"cpu_used_percent"`
		MemUsedBytes   int64     `json:"mem_used_bytes"`
	} `json:"sample"`
	Disks []struct {
		Mountpoint string `json:"mountpoint"`
		UsedBytes  int64  `json:"used_bytes"`
	} `json:"disks"`
	Networks []struct {
		Interface string  `json:"interface"`
		RxPerSec  float64 `json:"rx_bytes_per_sec"`
	} `json:"networks"`
}

// dialPanel abre el canal del panel con una cookie de sesión.
func (f *fixture) dialPanel(t *testing.T, cookie *http.Cookie) (*websocket.Conn, int, error) {
	t.Helper()

	url := strings.Replace(f.srv.URL, "http://", "ws://", 1) + "/v1/panel/connect"
	header := http.Header{}
	if cookie != nil {
		header.Set("Cookie", cookie.Name+"="+cookie.Value)
	}

	conn, resp, err := websocket.Dial(t.Context(), url, &websocket.DialOptions{
		HTTPClient: f.srv.Client(),
		HTTPHeader: header,
	})
	status := 0
	if resp != nil {
		status = resp.StatusCode
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	if conn != nil {
		t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	}
	return conn, status, err
}

// readPanel espera el próximo mensaje del canal.
func readPanel(t *testing.T, conn *websocket.Conn) panelMessage {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("leer del canal del panel: %v", err)
	}
	var message panelMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		t.Fatalf("leer el mensaje %s: %v", raw, err)
	}
	return message
}

func writePanel(t *testing.T, conn *websocket.Conn, payload string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
		t.Fatalf("escribir en el canal del panel: %v", err)
	}
}

func TestPanelChannelNeedsASession(t *testing.T) {
	f := setup(t)

	if _, status, err := f.dialPanel(t, nil); err == nil || status != http.StatusUnauthorized {
		t.Errorf("sin cookie: estado %d, error %v; se esperaba 401", status, err)
	}
	forged := &http.Cookie{Name: "__Host-statussv_session", Value: "svses_inventado"}
	if _, status, err := f.dialPanel(t, forged); err == nil || status != http.StatusUnauthorized {
		t.Errorf("con cookie inventada: estado %d, error %v; se esperaba 401", status, err)
	}
}

// El camino completo: el panel se suscribe, el agent manda un lote y la muestra
// aparece en el canal del panel.
func TestPanelReceivesLiveSamples(t *testing.T) {
	f := setup(t)
	cookie := f.login(t, "198.51.100.40")
	agent, serverID := connectForMetrics(t, f, "vps-en-vivo", "203.0.113.50")

	panelConn, status, err := f.dialPanel(t, cookie)
	if err != nil {
		t.Fatalf("abrir el canal del panel: %v (estado %d)", err, status)
	}

	if welcome := readPanel(t, panelConn); welcome.Type != "welcome" || welcome.OfflineAfterSeconds != int(testOffline.Seconds()) {
		t.Fatalf("bienvenida = %+v", welcome)
	}

	// El servidor confirma la suscripción: recién ahí tiene sentido mandar el
	// lote, sin depender de cuándo la procesó.
	writePanel(t, panelConn, `{"type":"subscribe","server_ids":["`+serverID.String()+`"]}`)
	ack := readPanel(t, panelConn)
	if ack.Type != "subscribed" || len(ack.ServerIDs) != 1 || ack.ServerIDs[0] != serverID {
		t.Fatalf("confirmación = %+v", ack)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	sendEnvelope(t, agent, proto.Metrics{Samples: []proto.Sample{testSample(now.Add(-10 * time.Second)), testSample(now)}})

	message := readPanel(t, panelConn)
	if message.Type != "sample" || message.ServerID != serverID {
		t.Fatalf("mensaje = %+v; se esperaba una muestra de %s", message, serverID)
	}
	// Del lote sale la muestra más nueva.
	if !message.Sample.TS.Equal(now) {
		t.Errorf("muestra de %v, se esperaba la más nueva (%v)", message.Sample.TS, now)
	}
	if message.Sample.CPUUsedPercent != 10 || message.Sample.MemUsedBytes != 1<<29 {
		t.Errorf("valores = %+v", message.Sample)
	}
	if len(message.Disks) != 1 || message.Disks[0].Mountpoint != "/" {
		t.Errorf("discos = %+v", message.Disks)
	}
	// El lote trae dos muestras, así que ya hay un par y sale la tasa. Los dos
	// contadores son iguales, de modo que la tasa es cero.
	if len(message.Networks) != 1 || message.Networks[0].Interface != "eth0" {
		t.Errorf("red = %+v", message.Networks)
	}

	// Al desuscribirse deja de llegar: se manda un lote y lo próximo que llega
	// es la respuesta a un mensaje inválido, no la muestra.
	writePanel(t, panelConn, `{"type":"unsubscribe","server_ids":["`+serverID.String()+`"]}`)
	if off := readPanel(t, panelConn); off.Type != "subscribed" || len(off.ServerIDs) != 0 {
		t.Fatalf("confirmación de baja = %+v", off)
	}
	sendEnvelope(t, agent, proto.Metrics{Samples: []proto.Sample{testSample(now.Add(time.Second))}})
	writePanel(t, panelConn, `{"type":"inventado"}`)
	if next := readPanel(t, panelConn); next.Type != "error" {
		t.Errorf("después de desuscribirse llegó %+v", next)
	}
}

func TestPanelRejectsGarbage(t *testing.T) {
	f := setup(t)
	cookie := f.login(t, "198.51.100.41")

	conn, _, err := f.dialPanel(t, cookie)
	if err != nil {
		t.Fatalf("abrir el canal: %v", err)
	}
	if welcome := readPanel(t, conn); welcome.Type != "welcome" {
		t.Fatalf("bienvenida = %+v", welcome)
	}

	for _, payload := range []string{`no es json`, `{"type":"inventado"}`, `{"type":"subscribe","server_ids":["no-es-uuid"]}`} {
		writePanel(t, conn, payload)
		if got := readPanel(t, conn); got.Type != "error" {
			t.Errorf("con %q el servidor respondió %+v, se esperaba un error", payload, got)
		}
	}

	// Después de los errores el canal sigue sirviendo.
	writePanel(t, conn, `{"type":"subscribe","server_ids":[]}`)
	if got := readPanel(t, conn); got.Type != "subscribed" {
		t.Errorf("el canal dejó de responder: %+v", got)
	}
}
