// Package api expone la API HTTP del control plane: los chequeos de salud, el
// enrolamiento de agents, el login del panel y, más adelante, el WebSocket del
// panel y el MCP.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JuanBaez77/status-sv/server/internal/config"
	"github.com/JuanBaez77/status-sv/server/internal/enrollment"
	"github.com/JuanBaez77/status-sv/server/internal/metrics"
	"github.com/JuanBaez77/status-sv/server/internal/panel"
	"github.com/JuanBaez77/status-sv/server/internal/store"
	"github.com/JuanBaez77/status-sv/server/internal/users"
)

// maxBodyBytes acota lo que se lee de un pedido. Los mensajes del agent son
// chicos; un cuerpo más grande que esto es un error o un abuso.
const maxBodyBytes = 1 << 20 // 1 MiB

// readyTimeout limita lo que puede tardar el chequeo contra la base.
const readyTimeout = 2 * time.Second

// Server tiene lo que necesitan los handlers.
type Server struct {
	cfg           config.Config
	pool          *pgxpool.Pool
	queries       *store.Queries
	log           *slog.Logger
	enrollment    *enrollment.Service
	hub           *hub
	enrollLimiter *ipLimiter
	ingester      *metrics.Ingester
	users         *users.Service
	panels        *panelHub
	counters      *counterCache
	loginLimiter  *ipLimiter
}

// New arma el servidor de la API.
func New(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) *Server {
	return &Server{
		cfg:           cfg,
		pool:          pool,
		queries:       store.New(pool),
		log:           log,
		enrollment:    enrollment.NewService(pool, cfg.EnrollmentTTL, log),
		hub:           newHub(),
		enrollLimiter: newIPLimiter(enrollPerMinute, enrollBurst, limiterTTL),
		ingester:      metrics.NewIngester(pool),
		users:         users.NewService(pool, log),
		panels:        newPanelHub(),
		counters:      newCounterCache(),
		loginLimiter:  newIPLimiter(loginPerMinute, loginBurst, limiterTTL),
	}
}

// Handler devuelve el enrutador con todos los middlewares puestos.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("POST /v1/agents/enroll", s.handleEnroll)

	// Rutas del panel. CrossOriginProtection rechaza los pedidos que cambian
	// algo y vienen de otro sitio (Sec-Fetch-Site u Origin), además de lo que ya
	// hace SameSite=Strict en la cookie. Las rutas de los agents quedan afuera:
	// no son de un navegador.
	crossOrigin := http.NewCrossOriginProtection()
	mux.Handle("POST /v1/auth/login", crossOrigin.Handler(http.HandlerFunc(s.handleLogin)))
	mux.Handle("POST /v1/auth/logout", crossOrigin.Handler(http.HandlerFunc(s.handleLogout)))
	mux.Handle("GET /v1/auth/me", s.requireSession(http.HandlerFunc(s.handleMe)))
	mux.Handle("GET /v1/servers", s.requireSession(http.HandlerFunc(s.handleListServers)))
	mux.Handle("GET /v1/servers/{id}/metrics", s.requireSession(http.HandlerFunc(s.handleServerMetrics)))

	// Todo lo que no es API es el panel, que va último para no taparle ninguna
	// ruta. Una ruta de /v1 que no existe se contesta como API, en JSON: si
	// cayera en el panel, un error de integración devolvería una página HTML.
	mux.HandleFunc("/v1/", s.handleUnknownAPI)
	mux.Handle("/", panel.Handler(s.log))

	// El WebSocket va fuera del límite de tamaño por pedido: ese límite es para
	// cuerpos HTTP, y acá el tamaño se controla por mensaje con SetReadLimit.
	limited := http.MaxBytesHandler(mux, maxBodyBytes)

	root := http.NewServeMux()
	root.Handle("/", limited)
	root.HandleFunc("GET /v1/agents/connect", s.handleAgentConnect)
	// El WebSocket del panel también va fuera del límite por pedido, por lo
	// mismo que el de los agents.
	root.Handle("GET /v1/panel/connect", s.requireSession(http.HandlerFunc(s.handlePanelConnect)))

	return s.recoverPanic(s.logRequests(root))
}

// CloseAgents cierra las conexiones de los agents. Se llama al apagar, para que
// cada agent se entere y reconecte con backoff en vez de quedarse esperando.
func (s *Server) CloseAgents(reason string) {
	s.panels.closeAll()
	s.hub.closeAll(reason)
}

// ConnectedAgents dice cuántos agents hay conectados en este momento.
func (s *Server) ConnectedAgents() int {
	return s.hub.count()
}

// handleUnknownAPI contesta las rutas de /v1 que no existen.
func (s *Server) handleUnknownAPI(w http.ResponseWriter, r *http.Request) {
	writeJSON(r.Context(), w, http.StatusNotFound, errorResponse{
		Error:   "not_found",
		Message: "esa ruta no existe",
	})
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

// clientIP devuelve la IP real del cliente, mirando X-Forwarded-For solo si el
// pedido viene de un proxy de confianza.
func (s *Server) clientIP(r *http.Request) string {
	trusted := s.cfg.TrustedProxies
	if len(trusted) == 0 {
		// Una configuración armada a mano, como la de los tests, puede no traer
		// la lista: en ese caso vale el valor por defecto, que es solo loopback.
		trusted = config.DefaultTrustedProxies()
	}
	return clientIP(r, trusted)
}
