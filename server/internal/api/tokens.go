package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/server/internal/audit"
	"github.com/JuanBaez77/status-sv/server/internal/tokens"
	"github.com/JuanBaez77/status-sv/server/internal/users"
)

// Tokens del MCP desde el panel (ADR-025).
//
// Crear un token pide un código TOTP nuevo en el mismo pedido, como un deploy
// de producción: una sesión robada no alcanza para sacar una credencial. El
// valor del token viaja una sola vez, en la respuesta del POST. Revocar no
// pide código: frenar un token tiene que ser fácil.

// Límites de los pedidos.
const (
	tokenRequestsPerMinute = 10
	tokenRequestsBurst     = 5
	// maxTokenBody acota el pedido: la lista de proyectos es lo más largo.
	maxTokenBody = 8 << 10
	// maxTokenDays es el vencimiento más largo que se puede pedir.
	maxTokenDays = int(tokens.MaxTTL / (24 * time.Hour))
)

// tokenTarget es el tipo de objetivo de un token en audit_log.
const tokenTarget = "api_token"

// tokenProject es un proyecto de la lista de un token. Missing es un ID que ya
// no corresponde a ningún proyecto declarado: el token no lo alcanza.
type tokenProject struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name,omitempty"`
	ServerName  string    `json:"server_name,omitempty"`
	Environment string    `json:"environment,omitempty"`
	Missing     bool      `json:"missing,omitempty"`
}

// tokenView es un token como lo ve el panel: sin su valor ni su hash.
type tokenView struct {
	ID            uuid.UUID      `json:"id"`
	Name          string         `json:"name"`
	Prefix        string         `json:"prefix"`
	Status        tokens.Status  `json:"status"`
	Scopes        []string       `json:"scopes"`
	AllProjects   bool           `json:"all_projects"`
	Projects      []tokenProject `json:"projects"`
	CreatedByName string         `json:"created_by_name"`
	CreatedAt     time.Time      `json:"created_at"`
	ExpiresAt     time.Time      `json:"expires_at"`
	RevokedAt     *time.Time     `json:"revoked_at"`
	RevokedByName string         `json:"revoked_by_name,omitempty"`
	LastUsedAt    *time.Time     `json:"last_used_at"`
	LastUsedIP    string         `json:"last_used_ip,omitempty"`
}

type tokenListResponse struct {
	Tokens []tokenView `json:"tokens"`
	// MaxDays es el vencimiento más largo que acepta el control plane.
	MaxDays int `json:"max_days"`
	// DefaultDays es el que propone el panel.
	DefaultDays int `json:"default_days"`
	// Scopes es la lista cerrada, en su orden.
	Scopes []string `json:"scopes"`
}

type createTokenRequest struct {
	Name          string      `json:"name"`
	Scopes        []string    `json:"scopes"`
	ProjectIDs    []uuid.UUID `json:"project_ids"`
	ExpiresInDays int         `json:"expires_in_days"`
	TOTPCode      string      `json:"totp_code"`
}

type createTokenResponse struct {
	// Token es el valor en claro. No se puede volver a pedir.
	Token string    `json:"token"`
	Item  tokenView `json:"item"`
}

// handleListTokens devuelve todos los tokens, los más nuevos primero.
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)

	list, err := s.tokens.List(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudieron listar los tokens", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	names, err := s.projectNames(r)
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudieron leer los proyectos", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	now := time.Now()
	views := make([]tokenView, len(list))
	for i, tok := range list {
		views[i] = viewToken(tok, names, now)
	}
	known := tokens.KnownScopes()
	scopes := make([]string, len(known))
	for i, scope := range known {
		scopes[i] = string(scope)
	}
	writeJSON(ctx, w, http.StatusOK, tokenListResponse{
		Tokens:      views,
		MaxDays:     maxTokenDays,
		DefaultDays: int(tokens.DefaultTTL / (24 * time.Hour)),
		Scopes:      scopes,
	})
}

// handleCreateToken crea un token y devuelve su valor, una sola vez.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)
	ip := s.clientIP(r)
	session := SessionFrom(ctx)

	if !s.tokenLimiter.allow(ip) {
		writeJSON(ctx, w, http.StatusTooManyRequests, errorResponse{Error: "rate_limited", Message: "demasiados pedidos de tokens seguidos"})
		return
	}
	var req createTokenRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTokenBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el pedido no es un JSON válido"})
		return
	}
	if req.ExpiresInDays < 1 || req.ExpiresInDays > maxTokenDays {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el vencimiento tiene que ser de 1 a 90 días"})
		return
	}
	userID := session.UserID
	newToken := tokens.NewToken{
		Name:          req.Name,
		Scopes:        req.Scopes,
		ProjectIDs:    req.ProjectIDs,
		TTL:           time.Duration(req.ExpiresInDays) * 24 * time.Hour,
		CreatedBy:     &userID,
		CreatedByName: session.Username,
	}

	// Se valida antes de mirar el código: un nombre equivocado no tiene que
	// gastar uno.
	if err := s.tokens.Check(ctx, newToken); err != nil {
		if errors.Is(err, tokens.ErrInvalid) {
			writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: tokenErrorMessage(err)})
			return
		}
		s.log.ErrorContext(ctx, "no se pudo validar el token", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}

	if !s.totpLimiter.allow(ip) {
		tooManyTOTP(w, r)
		return
	}
	if err := s.users.ConfirmAction(ctx, session, req.TOTPCode); err != nil {
		if errors.Is(err, users.ErrInvalidCode) || errors.Is(err, users.ErrWrongStage) {
			s.record(ctx, audit.Entry{
				Actor: session.Username, ActorType: audit.ActorUser, Action: audit.ActionTokenCreate,
				TargetType: tokenTarget, TargetID: audit.Unknown, Result: "rejected", IP: ip,
				Detail: map[string]any{"motivo": "código TOTP inválido"},
			})
			writeJSON(ctx, w, http.StatusForbidden, errorResponse{Error: "invalid_code", Message: "hace falta un código nuevo de la app de autenticación"})
			return
		}
		s.log.ErrorContext(ctx, "no se pudo verificar el código", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}

	secret, tok, err := s.tokens.Create(ctx, newToken)
	if errors.Is(err, tokens.ErrInvalid) {
		// Solo si un proyecto dejó de existir entre la validación y ahora.
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: tokenErrorMessage(err)})
		return
	}
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo crear el token", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}

	s.record(ctx, audit.Entry{
		Actor: session.Username, ActorType: audit.ActorUser, Action: audit.ActionTokenCreate,
		TargetType: tokenTarget, TargetID: tok.ID.String(), Result: audit.ResultOK, IP: ip,
		Detail: tokenDetail(tok),
	})
	s.log.InfoContext(ctx, "token del MCP creado", "token", tok.Prefix, "nombre", tok.Name, "usuario", session.Username)

	names, err := s.projectNames(r)
	if err != nil {
		// El token ya existe: se devuelve igual, sin los nombres.
		s.log.WarnContext(ctx, "no se pudieron leer los proyectos", "error", err)
	}
	writeJSON(ctx, w, http.StatusCreated, createTokenResponse{
		Token: secret.Reveal(),
		Item:  viewToken(tok, names, time.Now()),
	})
}

// handleRevokeToken revoca un token. Deja de servir en el próximo pedido.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)
	ip := s.clientIP(r)
	session := SessionFrom(ctx)

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el ID del token no es un UUID"})
		return
	}
	tok, err := s.tokens.Revoke(ctx, id, session.Username)
	switch {
	case errors.Is(err, tokens.ErrNotFound):
		writeJSON(ctx, w, http.StatusNotFound, errorResponse{Error: "not_found", Message: "no existe ese token"})
		return
	case errors.Is(err, tokens.ErrAlreadyRevoked):
		writeJSON(ctx, w, http.StatusConflict, errorResponse{Error: "already_revoked", Message: "el token ya estaba revocado"})
		return
	case err != nil:
		s.log.ErrorContext(ctx, "no se pudo revocar el token", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}

	s.record(ctx, audit.Entry{
		Actor: session.Username, ActorType: audit.ActorUser, Action: audit.ActionTokenRevoke,
		TargetType: tokenTarget, TargetID: tok.ID.String(), Result: audit.ResultOK, IP: ip,
		Detail: map[string]any{"prefijo": tok.Prefix, "nombre": tok.Name},
	})
	s.log.InfoContext(ctx, "token del MCP revocado", "token", tok.Prefix, "usuario", session.Username)

	names, err := s.projectNames(r)
	if err != nil {
		s.log.WarnContext(ctx, "no se pudieron leer los proyectos", "error", err)
	}
	writeJSON(ctx, w, http.StatusOK, viewToken(tok, names, time.Now()))
}

// projectNames devuelve los proyectos declarados por ID, para mostrar la
// lista de un token con nombres.
func (s *Server) projectNames(r *http.Request) (map[uuid.UUID]tokenProject, error) {
	rows, err := s.queries.ListProjects(r.Context())
	if err != nil {
		return nil, err
	}
	names := make(map[uuid.UUID]tokenProject, len(rows))
	for _, row := range rows {
		names[row.ID] = tokenProject{ID: row.ID, Name: row.Name, ServerName: row.ServerName, Environment: row.Environment}
	}
	return names, nil
}

func viewToken(tok tokens.Token, names map[uuid.UUID]tokenProject, now time.Time) tokenView {
	scopes := make([]string, len(tok.Scopes))
	for i, scope := range tok.Scopes {
		scopes[i] = string(scope)
	}
	projects := make([]tokenProject, len(tok.ProjectIDs))
	for i, id := range tok.ProjectIDs {
		project, ok := names[id]
		if !ok {
			project = tokenProject{ID: id, Missing: true}
		}
		projects[i] = project
	}
	return tokenView{
		ID:            tok.ID,
		Name:          tok.Name,
		Prefix:        tok.Prefix,
		Status:        tok.Status(now),
		Scopes:        scopes,
		AllProjects:   tok.AllProjects(),
		Projects:      projects,
		CreatedByName: tok.CreatedByName,
		CreatedAt:     tok.CreatedAt,
		ExpiresAt:     tok.ExpiresAt,
		RevokedAt:     tok.RevokedAt,
		RevokedByName: tok.RevokedByName,
		LastUsedAt:    tok.LastUsedAt,
		LastUsedIP:    tok.LastUsedIP,
	}
}

// tokenDetail es lo que la auditoría guarda de un token creado: nunca su
// valor.
func tokenDetail(tok tokens.Token) map[string]any {
	scopes := make([]string, len(tok.Scopes))
	for i, scope := range tok.Scopes {
		scopes[i] = string(scope)
	}
	// "todos", o cuántos proyectos tiene la lista.
	var projects any = "todos"
	if !tok.AllProjects() {
		projects = len(tok.ProjectIDs)
	}
	return map[string]any{
		"prefijo":   tok.Prefix,
		"nombre":    tok.Name,
		"scopes":    scopes,
		"proyectos": projects,
		"vence":     tok.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

// tokenErrorMessage saca del error de validación el texto para el panel,
// sin el prefijo del error general.
func tokenErrorMessage(err error) string {
	return strings.TrimPrefix(err.Error(), tokens.ErrInvalid.Error()+": ")
}
