// Package api expone la API HTTP del control plane: los chequeos de salud, el
// enrolamiento de agents y, más adelante, el WebSocket del panel y el MCP.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JuanBaez77/status-sv/server/internal/config"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// maxBodyBytes acota lo que se lee de un pedido. Los mensajes del agent son
// chicos; un cuerpo más grande que esto es un error o un abuso.
const maxBodyBytes = 1 << 20 // 1 MiB

// readyTimeout limita lo que puede tardar el chequeo contra la base.
const readyTimeout = 2 * time.Second

// Server tiene lo que necesitan los handlers.
type Server struct {
	cfg     config.Config
	pool    *pgxpool.Pool
	queries *store.Queries
	log     *slog.Logger
}

// New arma el servidor de la API.
func New(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) *Server {
	return &Server{
		cfg:     cfg,
		pool:    pool,
		queries: store.New(pool),
		log:     log,
	}
}

// Handler devuelve el enrutador con todos los middlewares puestos.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	return s.recoverPanic(s.logRequests(http.MaxBytesHandler(mux, maxBodyBytes)))
}

// handleHealthz dice si el proceso está vivo. No toca la base a propósito: es lo
// que mira el supervisor para decidir si reiniciar, y una base caída no es
// motivo para reiniciar el servidor.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(r.Context(), w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz dice si el servidor puede atender de verdad, o sea si la base
// responde. Es lo que mira el balanceador antes de mandarle tráfico.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()

	if err := s.pool.Ping(ctx); err != nil {
		s.log.WarnContext(ctx, "la base no responde", "error", err)
		writeJSON(ctx, w, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable",
			"reason": "database",
		})
		return
	}

	writeJSON(ctx, w, http.StatusOK, map[string]string{"status": "ready"})
}

// writeJSON escribe una respuesta JSON. Si falla la escritura no hay nada que
// hacer con el error salvo registrarlo: el cliente ya se fue.
func writeJSON(ctx context.Context, w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.ErrorContext(ctx, "no se pudo escribir la respuesta", "error", err)
	}
}
