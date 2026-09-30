package projects

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/proto"
)

func spec(url string, expect int) proto.HealthcheckSpec {
	return proto.HealthcheckSpec{URL: url, ExpectStatus: expect, TimeoutSeconds: 1}
}

// checkValid prueba y revisa que el resultado cumpla el contrato.
func checkValid(t *testing.T, s proto.HealthcheckSpec) proto.HealthResult {
	t.Helper()
	got := NewChecker("test").Check(t.Context(), s)
	if err := got.Validate(); err != nil {
		t.Fatalf("el resultado no cumple el contrato: %v (%+v)", err, got)
	}
	if got.CheckedAt.IsZero() || got.CheckedAt.Location() != time.UTC {
		t.Errorf("checked_at = %v", got.CheckedAt)
	}
	return got
}

func TestCheckPassing(t *testing.T) {
	var agent atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent.Store(r.UserAgent())
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	got := checkValid(t, spec(srv.URL+"/health", 200))
	if got.State != proto.HealthPassing || got.StatusCode != 200 || got.Detail != "" {
		t.Errorf("resultado = %+v", got)
	}
	if ua, _ := agent.Load().(string); ua != "status-sv-agent/test" {
		t.Errorf("User-Agent = %q", ua)
	}
}

func TestCheckUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("DATABASE_URL=postgres://secreto"))
	}))
	defer srv.Close()

	got := checkValid(t, spec(srv.URL, 200))
	if got.State != proto.HealthFailing || got.StatusCode != 503 {
		t.Errorf("resultado = %+v", got)
	}
	if got.Detail != "respondió 503 y se esperaba 200" {
		t.Errorf("detalle = %q", got.Detail)
	}
}

func TestCheckDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		followed.Add(1)
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusMovedPermanently)
	}))
	defer srv.Close()

	// Un 301 vale como 301: si se espera 200, está mal; si se espera 301, bien.
	if got := checkValid(t, spec(srv.URL, 200)); got.State != proto.HealthFailing || got.StatusCode != 301 {
		t.Errorf("esperando 200 = %+v", got)
	}
	if got := checkValid(t, spec(srv.URL, 301)); got.State != proto.HealthPassing {
		t.Errorf("esperando 301 = %+v", got)
	}
	if n := followed.Load(); n != 0 {
		t.Errorf("siguió la redirección %d veces", n)
	}
}

func TestCheckTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	got := checkValid(t, spec(srv.URL, 200))
	if got.State != proto.HealthFailing || got.StatusCode != 0 || got.Detail != "no respondió en 1 s" {
		t.Errorf("resultado = %+v", got)
	}
	if got.LatencyMS < 900 || got.LatencyMS > 5000 {
		t.Errorf("latencia = %d ms", got.LatencyMS)
	}
}

func TestCheckConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	got := checkValid(t, spec("http://"+addr+"/health", 200))
	if got.State != proto.HealthFailing || got.Detail != "conexión rechazada" {
		t.Errorf("resultado = %+v", got)
	}
}

func TestCheckVerifiesCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	got := checkValid(t, spec(srv.URL, 200))
	if got.State != proto.HealthFailing || got.Detail != "certificado inválido" {
		t.Errorf("resultado = %+v", got)
	}
}

// fakeChecker devuelve el estado que se le fije, con una latencia distinta en
// cada prueba para ver que eso solo no hace salir una foto.
type fakeChecker struct {
	mu     sync.Mutex
	state  proto.HealthState
	checks int
}

func (f *fakeChecker) set(state proto.HealthState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = state
}

func (f *fakeChecker) Check(_ context.Context, s proto.HealthcheckSpec) proto.HealthResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	r := proto.HealthResult{State: f.state, CheckedAt: t0.Add(time.Duration(f.checks) * time.Minute), LatencyMS: f.checks}
	if f.state == proto.HealthPassing {
		r.StatusCode = s.ExpectStatus
	} else {
		r.Detail = "conexión rechazada"
	}
	return r
}

func TestRunnerSendsHealthOnlyWhenStateChanges(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "cristales-baez.yaml", cristales+"healthcheck:\n  url: https://cristales.example.com/health\n")
	write(t, dir, "facturas.yaml", facturas)
	box := NewOutbox()
	checker := &fakeChecker{state: proto.HealthPassing}
	r := NewRunner(dir, box, checker, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := t0
	r.now = func() time.Time { return now }

	next := func() (proto.ProjectsState, bool) {
		t.Helper()
		s, ok := box.Next(now)
		if ok {
			if err := s.Validate(); err != nil {
				t.Fatalf("la foto no cumple el contrato: %v", err)
			}
			box.Sent([16]byte{1}, s.TakenAt, now)
			box.Acked(s.TakenAt)
		}
		return s, ok
	}
	health := func(s proto.ProjectsState) *proto.HealthResult {
		for _, p := range s.Projects {
			if p.Name == "cristales-baez" {
				return p.Health
			}
		}
		t.Fatal("falta cristales-baez")
		return nil
	}

	r.take(t.Context())
	if s, ok := next(); !ok || health(s) != nil {
		t.Fatalf("la primera lectura, antes de probar = %+v, %v", s, ok)
	}

	// La primera prueba sale.
	r.checkHealth(t.Context())
	s, ok := next()
	if !ok || health(s) == nil || health(s).State != proto.HealthPassing {
		t.Fatalf("después de la primera prueba = %+v, %v", s, ok)
	}
	for _, p := range s.Projects {
		if p.Name == "facturas" && p.Health != nil {
			t.Error("facturas no declara healthcheck y trae resultado")
		}
	}

	// Sigue bien, con otra latencia: no sale nada.
	now = now.Add(DefaultHealthInterval)
	r.checkHealth(t.Context())
	r.take(t.Context())
	if s, ok := next(); ok {
		t.Errorf("salió una foto sin cambio de estado: %+v", health(s))
	}

	// Pasa a mal: sale enseguida.
	checker.set(proto.HealthFailing)
	now = now.Add(DefaultHealthInterval)
	r.checkHealth(t.Context())
	s, ok = next()
	if !ok || health(s).State != proto.HealthFailing || health(s).Detail != "conexión rechazada" {
		t.Fatalf("después de caer = %+v, %v", s, ok)
	}

	// Sigue mal: nada, hasta el reenvío de los 5 minutos, que trae la última hora.
	now = now.Add(DefaultHealthInterval)
	r.checkHealth(t.Context())
	if _, ok := next(); ok {
		t.Error("salió una foto sin cambio de estado")
	}
	now = now.Add(DefaultResend)
	r.checkHealth(t.Context())
	s, ok = next()
	if !ok || !health(s).CheckedAt.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("el reenvío = %+v, %v", s, ok)
	}

	// Si cambia la URL, el resultado viejo no se usa hasta volver a probar.
	write(t, dir, "cristales-baez.yaml", cristales+"healthcheck:\n  url: https://cristales.example.com/otra\n")
	r.take(t.Context())
	if s, ok := next(); !ok || health(s) != nil {
		t.Errorf("con la URL cambiada = %+v, %v", s, ok)
	}
}

func TestRunnerIgnoresChecksCutByShutdown(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "cristales-baez.yaml", cristales+"healthcheck:\n  url: https://cristales.example.com/health\n")
	box := NewOutbox()
	r := NewRunner(dir, box, &fakeChecker{state: proto.HealthFailing}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.take(t.Context())

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.checkHealth(ctx)
	for _, p := range r.Current() {
		if p.Health != nil {
			t.Errorf("guardó una prueba cortada al apagar: %+v", p.Health)
		}
	}
}

func TestFailureDetailFitsContract(t *testing.T) {
	for _, err := range []error{io.ErrUnexpectedEOF, context.Canceled, &net.DNSError{Err: strings.Repeat("x", 500)}} {
		d := failureDetail(err, 30)
		if err := (proto.HealthResult{State: proto.HealthFailing, CheckedAt: t0, Detail: d}).Validate(); err != nil {
			t.Errorf("%q: %v", d, err)
		}
	}
}
