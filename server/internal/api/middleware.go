package api

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// statusRecorder recuerda el código de estado para poder registrarlo.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// logRequests deja una línea por pedido. No registra ni el cuerpo ni los
// encabezados: ahí viajan los tokens.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(recorder, r)

		// Los archivos del panel se piden una vez por visita y no cuentan nada:
		// a nivel debug, para que el registro siga siendo legible. Si alguno
		// falla, eso sí se ve.
		level := slog.LevelInfo
		if recorder.status < 400 && strings.HasPrefix(r.URL.Path, "/assets/") {
			level = slog.LevelDebug
		}

		s.log.Log(r.Context(), level, "pedido",
			"metodo", r.Method,
			"ruta", r.URL.Path,
			"estado", recorder.status,
			"duracion_ms", time.Since(start).Milliseconds(),
		)
	})
}

// recoverPanic evita que un panic en un handler tumbe todo el control plane.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.ErrorContext(ctx, "panic en un handler",
					"error", recovered,
					"ruta", r.URL.Path,
				)
				w.Header().Set("Connection", "close")
				writeJSON(ctx, w, http.StatusInternalServerError, map[string]string{
					"error": "internal",
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Unwrap le permite a http.ResponseController llegar al ResponseWriter real.
// Sin esto, el handshake del WebSocket falla con 501: envolver el writer le
// esconde a la librería la capacidad de tomar la conexión.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Hijack delega en el ResponseWriter real, para los clientes que hacen la
// aserción de tipo directamente en vez de usar http.ResponseController.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("el ResponseWriter no soporta Hijack: %T", r.ResponseWriter)
	}
	return hijacker.Hijack()
}

// Flush delega en el ResponseWriter real, para las respuestas que se mandan de a
// pedazos.
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
