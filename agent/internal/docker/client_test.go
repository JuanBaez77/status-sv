package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	idWeb = strings.Repeat("a1", 32)
	idDB  = strings.Repeat("b2", 32)
)

// fakeDocker imita las tres rutas que usa el cliente, con respuestas armadas
// como las de la API de Docker.
type fakeDocker struct {
	mu        sync.Mutex
	list      []map[string]any
	inspect   map[string]map[string]any
	inspected map[string]int
	events    []Event
	eventsReq *http.Request
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		list: []map[string]any{
			{
				"Id": idWeb, "Names": []string{"/cristales-web-1"}, "Image": "ghcr.io/extendsystem/cristales:2026.09.20",
				"State": "running", "Status": "Up 3 hours (healthy)",
				"Labels": map[string]string{labelComposeProject: "cristales", labelComposeService: "web"},
			},
			{
				"Id": idDB, "Names": []string{"/cristales-db-1", "/cristales-web-1/db"}, "Image": "postgres:17",
				"State": "exited", "Status": "Exited (0) 2 days ago", "Labels": map[string]string{},
			},
		},
		inspect: map[string]map[string]any{
			idWeb: {"State": map[string]any{
				"Status": "running", "StartedAt": "2026-09-27T09:00:00.123456789Z",
				"Health": map[string]any{"Status": "healthy"},
			}},
			idDB: {"State": map[string]any{"Status": "exited", "StartedAt": "0001-01-01T00:00:00Z"}},
		},
		inspected: make(map[string]int),
	}
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.URL.Path == "/containers/json":
		if r.URL.Query().Get("all") != "1" {
			http.Error(w, `{"message":"falta all=1"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(f.list)

	case strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/json")
		f.inspected[id]++
		body, ok := f.inspect[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"message":"No such container: %s"}`, id)
			return
		}
		_ = json.NewEncoder(w).Encode(body)

	case r.URL.Path == "/events":
		f.eventsReq = r
		w.WriteHeader(http.StatusOK)
		for _, e := range f.events {
			_ = json.NewEncoder(w).Encode(e)
		}
		// Al volver, el servidor cierra la respuesta: el stream se corta.

	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDocker) inspections(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inspected[id]
}

func startFake(t *testing.T, f *fakeDocker) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return newClient(srv.Client(), srv.URL)
}

func TestList(t *testing.T) {
	client := startFake(t, newFakeDocker())
	containers, err := client.List(context.Background())
	if err != nil {
		t.Fatalf("listar: %v", err)
	}
	if len(containers) != 2 {
		t.Fatalf("esperaba 2 contenedores, hay %d", len(containers))
	}
	web := containers[0]
	if web.ID != idWeb || web.State != "running" || web.Labels[labelComposeService] != "web" {
		t.Errorf("contenedor mal leído: %+v", web)
	}
}

func TestInspect(t *testing.T) {
	client := startFake(t, newFakeDocker())
	inspection, err := client.Inspect(context.Background(), idWeb)
	if err != nil {
		t.Fatalf("inspeccionar: %v", err)
	}
	if inspection.State.Health == nil || inspection.State.Health.Status != "healthy" {
		t.Errorf("healthcheck mal leído: %+v", inspection.State.Health)
	}
	if !inspection.State.StartedAt.Equal(time.Date(2026, 9, 27, 9, 0, 0, 123456789, time.UTC)) {
		t.Errorf("hora de arranque mal leída: %v", inspection.State.StartedAt)
	}
}

func TestInspectMissingIsNotFound(t *testing.T) {
	client := startFake(t, newFakeDocker())
	_, err := client.Inspect(context.Background(), strings.Repeat("c3", 32))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("esperaba ErrNotFound, obtuve %v", err)
	}
}

func TestErrorStatusCarriesDockerMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"el demonio está de mal humor"}`))
	}))
	defer srv.Close()

	_, err := newClient(srv.Client(), srv.URL).List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "mal humor") {
		t.Fatalf("el error tendría que traer el código y el mensaje de Docker: %v", err)
	}
}

func TestOversizedResponseIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("["))
		chunk := []byte(strings.Repeat(" ", 1<<20))
		for range maxResponseBytes>>20 + 1 {
			_, _ = w.Write(chunk)
		}
		_, _ = w.Write([]byte("]"))
	}))
	defer srv.Close()

	if _, err := newClient(srv.Client(), srv.URL).List(context.Background()); err == nil {
		t.Fatal("una respuesta de más de 8 MB tendría que rechazarse")
	}
}

func TestEventsStream(t *testing.T) {
	fake := newFakeDocker()
	fake.events = []Event{
		{Type: "container", Action: "die", Actor: struct {
			ID string `json:"ID"`
		}{ID: idWeb}},
		{Type: "container", Action: "start", Actor: struct {
			ID string `json:"ID"`
		}{ID: idWeb}},
	}
	client := startFake(t, fake)

	opened := 0
	var got []string
	err := client.Events(context.Background(), func() { opened++ }, func(e Event) { got = append(got, e.Action) })
	if err == nil {
		t.Fatal("el stream no termina sin error")
	}
	if opened != 1 {
		t.Errorf("onOpen se llamó %d veces", opened)
	}
	if strings.Join(got, ",") != "die,start" {
		t.Errorf("eventos recibidos: %v", got)
	}

	// Los filtros piden solo contenedores y dejan afuera los exec de los
	// healthchecks, que llegarían cada pocos segundos sin que cambie nada.
	var filters map[string][]string
	fake.mu.Lock()
	rawFilters := fake.eventsReq.URL.Query().Get("filters")
	fake.mu.Unlock()
	if err := json.Unmarshal([]byte(rawFilters), &filters); err != nil {
		t.Fatalf("leer los filtros: %v", err)
	}
	if strings.Join(filters["type"], ",") != "container" {
		t.Errorf("filtro de tipo: %v", filters["type"])
	}
	for _, action := range filters["event"] {
		if strings.HasPrefix(action, "exec") {
			t.Errorf("el filtro no tendría que pedir %q", action)
		}
	}
}

func TestEventsStopsWithContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- newClient(srv.Client(), srv.URL).Events(ctx, cancel, func(Event) {})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("esperaba context.Canceled, obtuve %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Events no volvió al cancelar el context")
	}
}

func TestMissingSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("los errores de sockets unix son de Linux")
	}
	client := NewClient(filepath.Join(t.TempDir(), "no-existe.sock"))
	_, err := client.List(context.Background())
	if !errors.Is(err, ErrNoSocket) {
		t.Fatalf("esperaba ErrNoSocket, obtuve %v", err)
	}
}

// Contra el Docker de verdad, si la máquina que corre el test lo tiene (el
// runner de CI sí). Solo lee.
func TestRealDocker(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("el socket de Docker es de Linux")
	}
	if _, err := os.Stat(DefaultSocket); err != nil {
		t.Skipf("no hay Docker en esta máquina: %v", err)
	}

	containers, err := NewClient(DefaultSocket).List(context.Background())
	if errors.Is(err, ErrPermissionDenied) {
		t.Skipf("hay Docker pero este usuario no tiene permiso: %v", err)
	}
	if err != nil {
		t.Fatalf("listar contra el Docker real: %v", err)
	}
	t.Logf("el Docker real tiene %d contenedores", len(containers))
}
