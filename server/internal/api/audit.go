package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/JuanBaez77/status-sv/server/internal/audit"
)

// blockedNoteEvery es cada cuánto un bloqueo por límite de intentos deja un
// renglón de auditoría para la misma IP y la misma acción (ADR-020): quien
// golpea el login no escribe un renglón por pedido en una tabla que no se borra.
const blockedNoteEvery = 15 * time.Minute

// maxBlockedNotes acota el mapa de lo ya anotado. Si se llena con IPs
// distintas, las que sobran no dejan renglón hasta que venzan las anteriores.
const maxBlockedNotes = 4096

// auditQueryParams son los parámetros que acepta GET /v1/audit. Uno
// desconocido es un error: un filtro mal escrito no puede devolver todo.
var auditQueryParams = map[string]bool{
	"actor_type": true, "actor": true, "action": true, "target_type": true, "target_id": true,
	"result": true, "from": true, "to": true, "before": true, "limit": true,
}

type auditResponse struct {
	Entries []audit.Row `json:"entries"`
	// NextCursor va en `before` para pedir la página siguiente. Falta cuando
	// no hay más.
	NextCursor string `json:"next_cursor,omitempty"`
}

// handleListAudit devuelve la auditoría, del renglón más nuevo al más viejo,
// con filtros y un cursor (ADR-020). Leerla no se audita.
func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)

	query := r.URL.Query()
	for name, values := range query {
		if !auditQueryParams[name] || len(values) != 1 {
			writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "parámetro desconocido o repetido"})
			return
		}
	}

	filter := audit.Filter{
		ActorType:  query.Get("actor_type"),
		Actor:      query.Get("actor"),
		Action:     query.Get("action"),
		TargetType: query.Get("target_type"),
		TargetID:   query.Get("target_id"),
		Result:     query.Get("result"),
		Before:     query.Get("before"),
	}
	var err error
	if filter.From, err = optionalTime(query.Get("from")); err == nil {
		filter.To, err = optionalTime(query.Get("to"))
	}
	if err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "from y to son instantes en formato RFC 3339"})
		return
	}
	if raw := query.Get("limit"); raw != "" {
		if filter.Limit, err = strconv.Atoi(raw); err != nil || filter.Limit < 1 {
			writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "limit es un número de 1 a 100"})
			return
		}
	}

	page, err := s.auditReader.List(ctx, filter)
	switch {
	case errors.Is(err, audit.ErrInvalidFilter):
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: err.Error()})
		return
	case err != nil:
		s.log.ErrorContext(ctx, "no se pudo leer la auditoría", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	writeJSON(ctx, w, http.StatusOK, auditResponse{Entries: page.Rows, NextCursor: page.Next})
}

func optionalTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, raw)
}

// recordAccess anota una acción de acceso de una persona sobre su propia
// cuenta: el actor y el objetivo son el mismo usuario.
func (s *Server) recordAccess(ctx context.Context, action, username, result, ip string, detail map[string]any) {
	if username == "" {
		username = audit.Unknown
	}
	s.record(ctx, audit.Entry{
		Actor: username, ActorType: audit.ActorUser, Action: action,
		TargetType: "user", TargetID: username, Result: result, IP: ip, Detail: detail,
	})
}

// recordBlocked anota un pedido frenado por el límite de intentos, como mucho
// una vez cada blockedNoteEvery por IP y por acción.
func (s *Server) recordBlocked(ctx context.Context, e audit.Entry) {
	if !s.blockedNotes.first(e.Action + "|" + e.IP) {
		return
	}
	e.Result = audit.ResultBlocked
	s.record(ctx, e)
}

// sparseNotes recuerda qué se anotó hace poco, para no anotarlo de nuevo.
type sparseNotes struct {
	mu    sync.Mutex
	last  map[string]time.Time
	every time.Duration
	limit int
	now   func() time.Time // se reemplaza en los tests
}

func newSparseNotes(every time.Duration, limit int) *sparseNotes {
	return &sparseNotes{last: make(map[string]time.Time), every: every, limit: limit, now: time.Now}
}

// first dice si key no se anotó en el último período, y la da por anotada.
func (n *sparseNotes) first(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	now := n.now()
	if at, ok := n.last[key]; ok && now.Sub(at) < n.every {
		return false
	}
	if len(n.last) >= n.limit {
		for k, at := range n.last {
			if now.Sub(at) >= n.every {
				delete(n.last, k)
			}
		}
		if len(n.last) >= n.limit {
			return false
		}
	}
	n.last[key] = now
	return true
}
