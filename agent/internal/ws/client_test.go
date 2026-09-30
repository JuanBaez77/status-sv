package ws

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/agent/internal/metrics"
	"github.com/JuanBaez77/status-sv/agent/internal/state"
	"github.com/JuanBaez77/status-sv/proto"
)

// fakeControlPlane es un control plane de mentira: acepta el canal, guarda los
// latidos que recibe y contesta el acuse.
type fakeControlPlane struct {
	srv *httptest.Server

	mu          sync.Mutex
	heartbeats  []proto.Heartbeat
	connections int

	// conns son los canales abiertos, para poder cortarlos a propósito.
	conns []*websocket.Conn

	// unauthorized hace que rechace la credencial.
	unauthorized bool
	// received se cierra al llegar el primer latido.
	received  chan struct{}
	closeOnce sync.Once

	// metricsMode dice qué hacer con un lote de métricas: confirmarlo
	// (el valor por defecto), rechazarlo o no contestar.
	metricsMode metricsMode
	// samples son todas las muestras recibidas, repetidas incluidas.
	samples         []proto.Sample
	metricsMessages int

	// runtimeStates son las fotos de runtime recibidas; runtimeMode dice qué
	// contestarles, igual que con las métricas.
	runtimeStates []proto.RuntimeState
	runtimeMode   metricsMode
	// projectsStates son los proyectos recibidos, que se confirman siempre.
	projectsStates []proto.ProjectsState

	// acks recibe las respuestas del agent a los comandos.
	acks chan proto.CommandAck
	// deployEvents son las fotos de deploy recibidas, que se confirman siempre.
	deployEvents []proto.DeployEvent
}

type metricsMode int

const (
	metricsAck metricsMode = iota
	// metricsReject contesta como el control plane del Sprint 0, que todavía
	// no guarda métricas.
	metricsReject
	metricsSilent
)

func newFakeControlPlane(t *testing.T) *fakeControlPlane {
	t.Helper()

	fake := &fakeControlPlane{received: make(chan struct{})}
	fake.srv = httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.srv.Close)

	return fake
}

func (f *fakeControlPlane) handle(w http.ResponseWriter, r *http.Request) {
	if f.isUnauthorized() {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Header.Get("Authorization") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(websocket.StatusInternalError, "") }()
	// El mismo tope que el control plane de verdad: un mensaje más grande
	// corta el canal.
	conn.SetReadLimit(proto.MaxMessageBytes)

	f.mu.Lock()
	f.conns = append(f.conns, conn)
	f.connections++
	f.mu.Unlock()

	for {
		_, raw, err := conn.Read(r.Context())
		if err != nil {
			return
		}

		envelope, err := proto.ParseEnvelope(raw)
		if err != nil {
			continue
		}
		if envelope.Type == proto.TypeCommandAck {
			if ack, err := proto.Decode[proto.CommandAck](envelope); err == nil {
				f.mu.Lock()
				acks := f.acks
				f.mu.Unlock()
				if acks != nil {
					acks <- ack
				}
			}
			continue
		}
		if envelope.Type == proto.TypeDeployEvent {
			ev, err := proto.Decode[proto.DeployEvent](envelope)
			if err != nil {
				continue
			}
			f.mu.Lock()
			f.deployEvents = append(f.deployEvents, ev)
			f.mu.Unlock()
			ack, err := proto.Marshal(proto.DeployEventAck{DeployID: ev.DeployID, Seq: ev.Seq})
			if err != nil || conn.Write(r.Context(), websocket.MessageText, ack) != nil {
				return
			}
			continue
		}
		if envelope.Type == proto.TypeProjectsState {
			if err := f.handleProjects(r.Context(), conn, envelope); err != nil {
				return
			}
			continue
		}
		if envelope.Type == proto.TypeRuntimeState {
			if err := f.handleRuntime(r.Context(), conn, envelope); err != nil {
				return
			}
			continue
		}
		if envelope.Type == proto.TypeMetrics {
			if err := f.handleMetrics(r.Context(), conn, envelope); err != nil {
				return
			}
			continue
		}
		if envelope.Type != proto.TypeHeartbeat {
			continue
		}

		heartbeat, err := proto.Decode[proto.Heartbeat](envelope)
		if err != nil {
			continue
		}

		f.mu.Lock()
		f.heartbeats = append(f.heartbeats, heartbeat)
		f.mu.Unlock()
		f.closeOnce.Do(func() { close(f.received) })

		ack, err := proto.Marshal(proto.HeartbeatAck{ServerTime: time.Now().UTC()})
		if err != nil {
			return
		}
		if err := conn.Write(r.Context(), websocket.MessageText, ack); err != nil {
			return
		}
	}
}

func (f *fakeControlPlane) isUnauthorized() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unauthorized
}

func (f *fakeControlPlane) setUnauthorized(value bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unauthorized = value
}

func (f *fakeControlPlane) lastHeartbeat() (proto.Heartbeat, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.heartbeats) == 0 {
		return proto.Heartbeat{}, false
	}
	return f.heartbeats[len(f.heartbeats)-1], true
}

func newTestClient(t *testing.T, fake *fakeControlPlane) *Client {
	t.Helper()

	client := NewClient(state.State{
		ServerURL:  fake.srv.URL,
		ServerID:   uuid.MustParse("01924f9c-0000-7000-8000-0000000000aa"),
		ServerName: "vps-01",
		Credential: "svagt_credencial-de-prueba",
	}, "0.1.0", slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.NewBuffer(metrics.DefaultMaxAge, metrics.DefaultMaxSamples))

	client.httpClient = fake.srv.Client()
	// Backoff corto para que los tests no tarden.
	client.backoff = &Backoff{Base: 10 * time.Millisecond, Max: 50 * time.Millisecond}

	return client
}

func TestClientSendsHeartbeatOnConnect(t *testing.T) {
	fake := newFakeControlPlane(t)
	client := newTestClient(t, fake)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() { _ = client.Run(ctx) }()

	select {
	case <-fake.received:
	case <-time.After(5 * time.Second):
		t.Fatal("no llegó ningún latido en 5 segundos")
	}

	heartbeat, ok := fake.lastHeartbeat()
	if !ok {
		t.Fatal("no se registró el latido")
	}
	if heartbeat.AgentVersion != "0.1.0" {
		t.Errorf("versión del agent = %q", heartbeat.AgentVersion)
	}
	// El latido lleva el consumo propio, que es una meta del proyecto.
	if heartbeat.SelfMemoryBytes == 0 {
		t.Error("el latido no informó la memoria del agent")
	}
	if err := heartbeat.Validate(); err != nil {
		t.Errorf("el latido enviado no pasa la validación del protocolo: %v", err)
	}
}

// Si el control plane se cae, el agent tiene que volver solo cuando se levanta.
func TestClientReconnectsAfterOutage(t *testing.T) {
	fake := newFakeControlPlane(t)
	client := newTestClient(t, fake)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() { _ = client.Run(ctx) }()

	select {
	case <-fake.received:
	case <-time.After(5 * time.Second):
		t.Fatal("no llegó el primer latido")
	}

	// Se simula la caída rechazando conexiones nuevas y cortando la actual.
	fake.setUnauthorized(true)
	fake.dropConnections()

	// Y después vuelve.
	time.Sleep(100 * time.Millisecond)
	fake.setUnauthorized(false)

	deadline := time.After(10 * time.Second)
	for {
		fake.mu.Lock()
		connections := fake.connections
		fake.mu.Unlock()

		if connections >= 2 {
			return // reconectó
		}

		select {
		case <-deadline:
			t.Fatalf("el agent no reconectó: %d conexiones", connections)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestClientReportsRevokedCredential(t *testing.T) {
	fake := newFakeControlPlane(t)
	fake.setUnauthorized(true)

	client := newTestClient(t, fake)

	err := client.session(t.Context())

	var unauthorized *ErrUnauthorized
	if !errors.As(err, &unauthorized) {
		t.Fatalf("se esperaba ErrUnauthorized, se obtuvo %v", err)
	}
}

func TestClientStopsWhenContextIsCancelled(t *testing.T) {
	fake := newFakeControlPlane(t)
	client := newTestClient(t, fake)

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()

	select {
	case <-fake.received:
	case <-time.After(5 * time.Second):
		t.Fatal("no llegó el primer latido")
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run devolvió error al cancelar: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("el agent no se detuvo al cancelar el contexto")
	}
}

// dropConnections corta los canales abiertos, que es lo que pasa cuando el
// control plane se reinicia.
//
// No alcanza con httptest.Server.CloseClientConnections: una vez que la
// conexión se convierte en WebSocket, el servidor de pruebas deja de seguirla y
// no la cierra.
func (f *fakeControlPlane) dropConnections() {
	f.mu.Lock()
	conns := f.conns
	f.conns = nil
	f.mu.Unlock()

	for _, conn := range conns {
		_ = conn.Close(websocket.StatusGoingAway, "corte simulado")
	}
}

// muteControlPlane acepta el canal y no contesta nunca: simula la red que se
// corta sin que se cierre la conexión, que es lo que pasa detrás de un NAT.
type muteControlPlane struct {
	srv *httptest.Server

	mu          sync.Mutex
	connections int
}

func newMuteControlPlane(t *testing.T) *muteControlPlane {
	t.Helper()

	mute := &muteControlPlane{}
	mute.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(websocket.StatusInternalError, "") }()

		mute.mu.Lock()
		mute.connections++
		mute.mu.Unlock()

		// Se queda escuchando sin contestar jamás.
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(mute.srv.Close)

	return mute
}

func (m *muteControlPlane) connectionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connections
}

// Un control plane que acepta la conexión pero no contesta tiene que terminar
// en una reconexión: si no, el agent se queda colgado creyendo que está vivo y
// el panel lo muestra caído sin que nadie entienda por qué.
func TestClientGivesUpOnASilentControlPlane(t *testing.T) {
	mute := newMuteControlPlane(t)

	client := NewClient(state.State{
		ServerURL:  mute.srv.URL,
		ServerID:   uuid.MustParse("01924f9c-0000-7000-8000-0000000000aa"),
		ServerName: "vps-01",
		Credential: "svagt_credencial-de-prueba",
	}, "0.1.0", slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.NewBuffer(metrics.DefaultMaxAge, metrics.DefaultMaxSamples))

	client.httpClient = mute.srv.Client()
	client.backoff = &Backoff{Base: 10 * time.Millisecond, Max: 20 * time.Millisecond}
	client.silence = 150 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() { _ = client.Run(ctx) }()

	// Con un plazo de 150 ms, en dos segundos tiene que haber reconectado varias veces.
	deadline := time.After(5 * time.Second)
	for {
		if mute.connectionCount() >= 3 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("el agent siguió colgado del canal mudo: %d conexiones", mute.connectionCount())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
