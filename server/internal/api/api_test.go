package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JuanBaez77/status-sv/server/internal/config"
)

// newTestServer arma un servidor sin base: alcanza para todo lo que no la toca.
func newTestServer() *Server {
	return &Server{
		cfg: config.Config{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestHealthzDoesNotNeedTheDatabase(t *testing.T) {
	// El pool es nil a propósito: si /healthz tocara la base, esto entraría en pánico.
	server := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("estado %d, se esperaba 200", rec.Code)
	}
	if contentType := rec.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("Content-Type = %q", contentType)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("la respuesta no es JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("cuerpo = %v", body)
	}
}

func TestRouting(t *testing.T) {
	server := newTestServer()

	tests := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{name: "ruta inexistente", method: http.MethodGet, path: "/nada", want: http.StatusNotFound},
		{name: "método equivocado", method: http.MethodPost, path: "/healthz", want: http.StatusMethodNotAllowed},
		{name: "raíz", method: http.MethodGet, path: "/", want: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("estado %d, se esperaba %d", rec.Code, tt.want)
			}
		})
	}
}

// Un panic en un handler no puede tumbar el control plane entero.
func TestRecoverPanic(t *testing.T) {
	server := newTestServer()

	exploding := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("algo salió muy mal")
	})

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rec := httptest.NewRecorder()

	// Si el middleware no atrapara el panic, este test fallaría con el panic mismo.
	server.recoverPanic(exploding).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("estado %d, se esperaba 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "internal") {
		t.Errorf("cuerpo = %q", rec.Body.String())
	}
}

// El log de pedidos no puede llevarse por delante encabezados con secretos.
func TestRequestLogOmitsHeadersAndBody(t *testing.T) {
	var logged strings.Builder
	server := &Server{
		cfg: config.Config{},
		log: slog.New(slog.NewTextHandler(&logged, nil)),
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", strings.NewReader("cuerpo-secreto"))
	req.Header.Set("Authorization", "Bearer svagt_secreto_que_no_puede_aparecer")
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	out := logged.String()
	if strings.Contains(out, "svagt_secreto_que_no_puede_aparecer") {
		t.Errorf("el log filtró el encabezado de autorización: %s", out)
	}
	if strings.Contains(out, "cuerpo-secreto") {
		t.Errorf("el log filtró el cuerpo del pedido: %s", out)
	}
	if !strings.Contains(out, "/healthz") {
		t.Errorf("el log no registró la ruta: %s", out)
	}
}
