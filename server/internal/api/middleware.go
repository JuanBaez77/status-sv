package api

import (
	"net/http"
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

		s.log.InfoContext(r.Context(), "pedido",
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
