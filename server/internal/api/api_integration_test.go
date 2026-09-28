//go:build integration

// Flujo completo contra PostgreSQL de verdad: enrolar por HTTP, abrir el canal
// y latir. Es lo que va a hacer el agent del PR siguiente.
package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http/httptest"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/api"
	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/config"
	"github.com/JuanBaez77/status-sv/server/internal/enrollment"
	"github.com/JuanBaez77/status-sv/server/internal/store"
	"github.com/JuanBaez77/status-sv/server/internal/testdb"
)

const (
	testTTL     = 15 * time.Minute
	testOffline = 45 * time.Second
)

type fixture struct {
	srv     *httptest.Server
	pool    *pgxpool.Pool
	queries *store.Queries
	enroll  *enrollment.Service
	totpKey auth.TOTPKey
}

func setup(t *testing.T) *fixture {
	t.Helper()

	pool := testdb.Setup(t)
	testdb.Truncate(t, pool)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	totpKey, err := auth.NewTOTPKey()
	if err != nil {
		t.Fatalf("NewTOTPKey: %v", err)
	}
	cfg := config.Config{EnrollmentTTL: testTTL, OfflineAfter: testOffline, TOTPKey: totpKey}

	srv := httptest.NewServer(api.New(cfg, pool, log).Handler())
	t.Cleanup(srv.Close)

	return &fixture{
		srv:     srv,
		pool:    pool,
		queries: store.New(pool),
		enroll:  enrollment.NewService(pool, testTTL, log),
		totpKey: totpKey,
	}
}

// newToken crea un token de enrolamiento para un servidor.
func (f *fixture) newToken(t *testing.T, serverName string) string {
	t.Helper()

	secret, _, err := f.enroll.CreateToken(t.Context(), serverName, "test")
	if err != nil {
		t.Fatalf("crear el token: %v", err)
	}
	return secret.Reveal()
}

// postEnroll manda un enrolamiento. La IP simulada va en X-Forwarded-For, que el
// servidor acepta porque el pedido sale de loopback: así cada test tiene su
// propio cupo en el límite de frecuencia y no se pisan entre ellos.
func (f *fixture) postEnroll(t *testing.T, token, hostname, ip string) (int, proto.Envelope) {
	t.Helper()

	body, err := proto.Marshal(proto.Enroll{
		Token:        token,
		Hostname:     hostname,
		AgentVersion: "0.1.0",
		OS:           "linux",
		Arch:         "amd64",
	})
	if err != nil {
		t.Fatalf("armar el pedido de enrolamiento: %v", err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		f.srv.URL+"/v1/agents/enroll", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("armar el pedido HTTP: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)

	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("enviar el enrolamiento: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("leer la respuesta: %v", err)
	}

	var envelope proto.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("la respuesta no es un envelope: %v (%s)", err, raw)
	}
	return resp.StatusCode, envelope
}

// enrollOK enrola y devuelve la credencial, fallando si algo sale mal.
func (f *fixture) enrollOK(t *testing.T, serverName, ip string) proto.EnrollAck {
	t.Helper()

	token := f.newToken(t, serverName)
	status, envelope := f.postEnroll(t, token, serverName+".example.com", ip)
	if status != http.StatusOK {
		t.Fatalf("el enrolamiento devolvió %d: %s", status, envelope.Data)
	}

	ack, err := proto.Decode[proto.EnrollAck](envelope)
	if err != nil {
		t.Fatalf("leer el enroll_ack: %v", err)
	}
	return ack
}

// dial abre el WebSocket del agent con una credencial. Devuelve el código de
// estado del handshake, que es lo que interesa cuando el intento se rechaza.
func (f *fixture) dial(t *testing.T, credential string) (*websocket.Conn, int, error) {
	t.Helper()

	url := strings.Replace(f.srv.URL, "http://", "ws://", 1) + "/v1/agents/connect"

	conn, resp, err := websocket.Dial(t.Context(), url, &websocket.DialOptions{
		HTTPClient: f.srv.Client(),
		HTTPHeader: http.Header{"Authorization": {"Bearer " + credential}},
	})

	status := 0
	if resp != nil {
		status = resp.StatusCode
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	return conn, status, err
}

// send manda un mensaje del protocolo por el canal.
func send(t *testing.T, conn *websocket.Conn, message proto.Message) {
	t.Helper()

	raw, err := proto.Marshal(message)
	if err != nil {
		t.Fatalf("serializar %s: %v", message.Type(), err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("enviar %s: %v", message.Type(), err)
	}
}

// receive lee el próximo mensaje del canal.
func receive(t *testing.T, conn *websocket.Conn) proto.Envelope {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("leer del canal: %v", err)
	}

	envelope, err := proto.ParseEnvelope(raw)
	if err != nil {
		t.Fatalf("leer el envelope recibido: %v", err)
	}
	return envelope
}

// errorCode saca el código de un envelope de error.
func errorCode(t *testing.T, envelope proto.Envelope) proto.ErrorCode {
	t.Helper()

	message, err := proto.Decode[proto.ErrorMessage](envelope)
	if err != nil {
		t.Fatalf("leer el mensaje de error: %v", err)
	}
	return message.Code
}

// El camino feliz completo: token, enrolamiento, canal y latido.
func TestEnrollConnectAndHeartbeat(t *testing.T) {
	f := setup(t)
	ctx := t.Context()

	ack := f.enrollOK(t, "vps-01", "203.0.113.10")

	if ack.ServerName != "vps-01" {
		t.Errorf("el ack trae el servidor %q", ack.ServerName)
	}
	if !strings.HasPrefix(ack.Credential, "svagt_") {
		t.Errorf("la credencial no tiene el prefijo esperado")
	}
	if err := ack.Config.Validate(); err != nil {
		t.Errorf("la configuración inicial no es válida: %v", err)
	}

	// El servidor quedó registrado y el token, consumido.
	server, err := f.queries.GetServerByID(ctx, ack.ServerID)
	if err != nil {
		t.Fatalf("buscar el servidor recién enrolado: %v", err)
	}
	if server.LastSeenAt != nil {
		t.Error("el servidor no debería tener last_seen antes del primer latido")
	}

	pending, err := f.queries.ListPendingEnrollmentTokens(ctx)
	if err != nil {
		t.Fatalf("listar tokens pendientes: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("quedaron %d tokens sin usar", len(pending))
	}

	conn, _, err := f.dial(t, ack.Credential)
	if err != nil {
		t.Fatalf("abrir el canal: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "fin del test") }()

	send(t, conn, proto.Heartbeat{
		AgentVersion:    "0.2.0",
		UptimeSeconds:   42,
		SelfMemoryBytes: 21 << 20,
		SelfCPUPercent:  0.3,
	})

	envelope := receive(t, conn)
	if envelope.Type != proto.TypeHeartbeatAck {
		t.Fatalf("se recibió %q y se esperaba heartbeat_ack: %s", envelope.Type, envelope.Data)
	}
	ackMessage, err := proto.Decode[proto.HeartbeatAck](envelope)
	if err != nil {
		t.Fatalf("leer el heartbeat_ack: %v", err)
	}
	if time.Since(ackMessage.ServerTime) > time.Minute {
		t.Errorf("la hora del servidor vino desfasada: %v", ackMessage.ServerTime)
	}

	// Esto es lo que se mira para decir que el Sprint 0 está terminado.
	updated, err := f.queries.GetServerByID(ctx, ack.ServerID)
	if err != nil {
		t.Fatalf("releer el servidor: %v", err)
	}
	if updated.LastSeenAt == nil {
		t.Fatal("el latido no actualizó last_seen")
	}
	if time.Since(*updated.LastSeenAt) > time.Minute {
		t.Errorf("last_seen quedó viejo: %v", updated.LastSeenAt)
	}
	if updated.AgentVersion != "0.2.0" {
		t.Errorf("la versión del agent no se actualizó: %q", updated.AgentVersion)
	}
}

func TestEnrollRejectsTokenTwice(t *testing.T) {
	f := setup(t)

	token := f.newToken(t, "vps-01")

	status, _ := f.postEnroll(t, token, "vps01.example.com", "203.0.113.11")
	if status != http.StatusOK {
		t.Fatalf("el primer enrolamiento devolvió %d", status)
	}

	status, envelope := f.postEnroll(t, token, "vps01.example.com", "203.0.113.11")
	if status != http.StatusUnauthorized {
		t.Fatalf("el segundo enrolamiento devolvió %d, se esperaba 401", status)
	}
	if code := errorCode(t, envelope); code != proto.CodeInvalidToken {
		t.Errorf("código %q, se esperaba invalid_token", code)
	}
}

func TestEnrollRejectsExpiredToken(t *testing.T) {
	f := setup(t)

	token := f.newToken(t, "vps-01")

	// Se lo envejece a mano en vez de esperar quince minutos.
	if _, err := f.pool.Exec(t.Context(),
		"UPDATE enrollment_tokens SET expires_at = now() - interval '1 minute'"); err != nil {
		t.Fatalf("envejecer el token: %v", err)
	}

	status, envelope := f.postEnroll(t, token, "vps01.example.com", "203.0.113.12")
	if status != http.StatusUnauthorized {
		t.Fatalf("devolvió %d, se esperaba 401", status)
	}
	if code := errorCode(t, envelope); code != proto.CodeInvalidToken {
		t.Errorf("código %q, se esperaba invalid_token", code)
	}

	// Y no quedó ningún servidor a medio crear.
	servers, err := f.queries.ListServers(t.Context())
	if err != nil {
		t.Fatalf("listar servidores: %v", err)
	}
	if len(servers) != 0 {
		t.Errorf("se crearon %d servidores con un token vencido", len(servers))
	}
}

func TestEnrollRejectsGarbage(t *testing.T) {
	f := setup(t)

	tests := []struct {
		name string
		ip   string
		body string
		want int
	}{
		{name: "no es JSON", body: "hola", want: http.StatusBadRequest, ip: "203.0.113.20"},
		{name: "envelope sin versión", body: `{"type":"enroll","data":{}}`, want: http.StatusBadRequest, ip: "203.0.113.21"},
		{name: "tipo equivocado", body: `{"v":1,"type":"heartbeat","id":"01924f9c-0000-7000-8000-000000000001","ts":"2026-09-20T12:00:00Z","data":{"agent_version":"0.1.0"}}`, want: http.StatusBadRequest, ip: "203.0.113.22"},
		{name: "token vacío", body: `{"v":1,"type":"enroll","id":"01924f9c-0000-7000-8000-000000000001","ts":"2026-09-20T12:00:00Z","data":{"token":"","hostname":"h","agent_version":"0.1.0"}}`, want: http.StatusBadRequest, ip: "203.0.113.23"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				f.srv.URL+"/v1/agents/enroll", strings.NewReader(tt.body))
			if err != nil {
				t.Fatalf("armar el pedido: %v", err)
			}
			req.Header.Set("X-Forwarded-For", tt.ip)

			resp, err := f.srv.Client().Do(req)
			if err != nil {
				t.Fatalf("enviar el pedido: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.want {
				t.Errorf("estado %d, se esperaba %d", resp.StatusCode, tt.want)
			}
		})
	}
}

// Reinstalar un VPS y enrolarlo de nuevo reutiliza el registro y deja una sola
// credencial válida: la vieja se revoca en la misma transacción.
func TestReenrollmentRotatesCredential(t *testing.T) {
	f := setup(t)
	ctx := t.Context()

	first := f.enrollOK(t, "vps-01", "203.0.113.30")
	second := f.enrollOK(t, "vps-01", "203.0.113.31")

	if first.ServerID != second.ServerID {
		t.Errorf("el segundo enrolamiento creó otro servidor: %v y %v", first.ServerID, second.ServerID)
	}
	if first.Credential == second.Credential {
		t.Error("se entregó la misma credencial dos veces")
	}

	servers, err := f.queries.ListServers(ctx)
	if err != nil {
		t.Fatalf("listar servidores: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("hay %d servidores con el mismo nombre, se esperaba 1", len(servers))
	}

	credentials, err := f.queries.ListCredentialsForServer(ctx, first.ServerID)
	if err != nil {
		t.Fatalf("listar credenciales: %v", err)
	}
	if len(credentials) != 2 {
		t.Fatalf("hay %d credenciales, se esperaban 2 (una revocada)", len(credentials))
	}

	var activas int
	for _, credential := range credentials {
		if credential.RevokedAt == nil {
			activas++
		}
	}
	if activas != 1 {
		t.Errorf("hay %d credenciales activas, se esperaba 1", activas)
	}

	// La credencial vieja ya no abre el canal.
	if conn, status, err := f.dial(t, first.Credential); err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		t.Error("la credencial revocada todavía abre el canal")
	} else if status != http.StatusUnauthorized {
		t.Errorf("el rechazo vino con estado %d, se esperaba 401", status)
	}

	// La nueva sí.
	conn, _, err := f.dial(t, second.Credential)
	if err != nil {
		t.Fatalf("la credencial nueva no abre el canal: %v", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "fin del test")
}

// Si el agent reconecta tras un corte, la conexión vieja que quedó colgada se
// cierra y queda la nueva.
func TestSecondConnectionReplacesTheFirst(t *testing.T) {
	f := setup(t)

	ack := f.enrollOK(t, "vps-01", "203.0.113.32")

	first, _, err := f.dial(t, ack.Credential)
	if err != nil {
		t.Fatalf("abrir la primera conexión: %v", err)
	}
	defer func() { _ = first.Close(websocket.StatusNormalClosure, "") }()

	second, _, err := f.dial(t, ack.Credential)
	if err != nil {
		t.Fatalf("abrir la segunda conexión: %v", err)
	}
	defer func() { _ = second.Close(websocket.StatusNormalClosure, "") }()

	// La primera tiene que haberse cerrado sola.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if _, _, err := first.Read(ctx); err == nil {
		t.Fatal("la conexión vieja sigue viva")
	} else if status := websocket.CloseStatus(err); status != websocket.StatusPolicyViolation {
		t.Errorf("la conexión vieja se cerró con estado %v, se esperaba policy violation", status)
	}

	// La segunda funciona.
	send(t, second, proto.Heartbeat{AgentVersion: "0.1.0"})
	if envelope := receive(t, second); envelope.Type != proto.TypeHeartbeatAck {
		t.Errorf("la conexión nueva respondió %q", envelope.Type)
	}
}

func TestConnectRejectsBadCredentials(t *testing.T) {
	f := setup(t)

	ack := f.enrollOK(t, "vps-01", "203.0.113.33")

	tests := []struct {
		name       string
		credential string
	}{
		{name: "credencial inventada", credential: "svagt_noexisteestacredencial1234567890"},
		{name: "un token de enrolamiento", credential: f.newToken(t, "vps-02")},
		{name: "texto cualquiera", credential: "hola"},
		{name: "credencial con un carácter cambiado", credential: ack.Credential[:len(ack.Credential)-1] + "X"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, status, err := f.dial(t, tt.credential)
			if err == nil {
				_ = conn.Close(websocket.StatusNormalClosure, "")
				t.Fatal("se abrió el canal con una credencial que no sirve")
			}
			if status != http.StatusUnauthorized {
				t.Errorf("se esperaba 401, vino %d", status)
			}
		})
	}
}

func TestConnectNeedsAuthorizationHeader(t *testing.T) {
	f := setup(t)

	url := strings.Replace(f.srv.URL, "http://", "ws://", 1) + "/v1/agents/connect"

	conn, resp, err := websocket.Dial(t.Context(), url, &websocket.DialOptions{HTTPClient: f.srv.Client()})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("se abrió el canal sin credencial")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("se esperaba 401, vino %v", resp)
	}
}

// El enrolamiento es el único endpoint que acepta un secreto sin autenticación
// previa, así que tiene que frenar a quien pruebe tokens en serie.
func TestEnrollIsRateLimited(t *testing.T) {
	f := setup(t)

	const ip = "203.0.113.40"
	invalid := "svenr_estetokennoexisteperoeslargo123456"

	var lastStatus int
	// La ráfaga permitida son 3 pedidos; con 6 seguidos el límite tiene que saltar.
	for range 6 {
		lastStatus, _ = f.postEnroll(t, invalid, "vps01.example.com", ip)
	}

	if lastStatus != http.StatusTooManyRequests {
		t.Fatalf("tras varios intentos el estado es %d, se esperaba 429", lastStatus)
	}

	// Otra IP no queda afectada por el castigo de la primera.
	status, _ := f.postEnroll(t, invalid, "vps01.example.com", "203.0.113.41")
	if status != http.StatusUnauthorized {
		t.Errorf("otra IP recibió %d, se esperaba 401", status)
	}
}

// La garantía de un solo uso tiene que valer también cuando dos pedidos llegan
// a la vez: es lo que pasaría si alguien interceptara el token y corriera a
// usarlo antes que el agent legítimo. Solo uno puede ganar, y tiene que quedar
// un único servidor con una única credencial activa.
func TestConcurrentEnrollmentsWithTheSameToken(t *testing.T) {
	f := setup(t)

	const intentos = 8
	token := f.newToken(t, "vps-01")

	type outcome struct {
		status int
		ack    proto.EnrollAck
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		outcomes []outcome
		largada  = make(chan struct{})
	)

	for i := range intentos {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-largada // todos arrancan juntos

			status, envelope := f.postEnroll(t, token, "vps01.example.com",
				fmt.Sprintf("203.0.113.%d", 50+i))

			result := outcome{status: status}
			if status == http.StatusOK {
				ack, err := proto.Decode[proto.EnrollAck](envelope)
				if err == nil {
					result.ack = ack
				}
			}

			mu.Lock()
			outcomes = append(outcomes, result)
			mu.Unlock()
		}(i)
	}

	close(largada)
	wg.Wait()

	var ganadores, rechazados int
	for _, result := range outcomes {
		switch result.status {
		case http.StatusOK:
			ganadores++
		case http.StatusUnauthorized:
			rechazados++
		default:
			t.Errorf("un intento terminó con estado inesperado: %d", result.status)
		}
	}

	if ganadores != 1 {
		t.Fatalf("%d intentos simultáneos lograron enrolarse; solo uno podía", ganadores)
	}
	if rechazados != intentos-1 {
		t.Errorf("%d intentos rechazados, se esperaban %d", rechazados, intentos-1)
	}

	servers, err := f.queries.ListServers(t.Context())
	if err != nil {
		t.Fatalf("listar servidores: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("quedaron %d servidores, se esperaba 1", len(servers))
	}

	credentials, err := f.queries.ListCredentialsForServer(t.Context(), servers[0].ID)
	if err != nil {
		t.Fatalf("listar credenciales: %v", err)
	}
	if len(credentials) != 1 {
		t.Errorf("quedaron %d credenciales, se esperaba 1", len(credentials))
	}
}
