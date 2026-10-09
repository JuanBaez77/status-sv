// Package tokens maneja los tokens del MCP (ADR-025): los crea, los verifica,
// los revoca y dice a qué alcanza cada uno.
//
// El valor en claro de un token existe una sola vez, en lo que devuelve
// Create. La base guarda su SHA-256 y sus primeros caracteres.
package tokens

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// Límites de un token. La migración 00013 exige los mismos.
const (
	// DefaultTTL es cuánto dura un token si no se elige otra cosa.
	DefaultTTL = 30 * 24 * time.Hour
	// MaxTTL es lo más que puede durar.
	MaxTTL = 90 * 24 * time.Hour
	// MaxNameLen es el largo máximo del nombre, en caracteres.
	MaxNameLen = 64
	// MaxProjects es el tope de la lista de proyectos.
	MaxProjects = 50
	// PrefixLen es cuántos caracteres del token se guardan para reconocerlo:
	// el prefijo fijo y seis al azar, que no alcanzan para adivinar el resto.
	PrefixLen = len(auth.MCPTokenPrefix) + 6

	// tokenLen es el largo de un token: el prefijo y 32 bytes en base64.
	tokenLen = len(auth.MCPTokenPrefix) + 43
	maxIPLen = 64
)

// Errores del paquete.
var (
	// ErrInvalid envuelve los errores de un token mal pedido: el mensaje se
	// le puede mostrar a quien lo pidió.
	ErrInvalid = errors.New("token inválido")
	// ErrUnauthenticated es un token que no existe, venció o fue revocado.
	// No se distingue cuál: quien lo manda no necesita saberlo.
	ErrUnauthenticated = errors.New("token desconocido, vencido o revocado")
	// ErrNotFound es un ID que no corresponde a ningún token.
	ErrNotFound = errors.New("el token no existe")
	// ErrAlreadyRevoked es revocar un token que ya lo estaba.
	ErrAlreadyRevoked = errors.New("el token ya estaba revocado")
)

// Status es el estado de un token, derivado de sus fechas.
type Status string

// Estados de un token.
const (
	StatusActive  Status = "active"
	StatusExpired Status = "expired"
	StatusRevoked Status = "revoked"
)

// Token es un token del MCP, sin su valor.
type Token struct {
	ID     uuid.UUID
	Name   string
	Prefix string
	Scopes []Scope
	// ProjectIDs vacío es que alcanza a todos los proyectos de sus scopes.
	ProjectIDs    []uuid.UUID
	CreatedByName string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	RevokedAt     *time.Time
	RevokedByName string
	LastUsedAt    *time.Time
	LastUsedIP    string
}

// Status dice si el token sirve, venció o fue revocado.
func (t Token) Status(now time.Time) Status {
	switch {
	case t.RevokedAt != nil:
		return StatusRevoked
	case !now.Before(t.ExpiresAt):
		return StatusExpired
	default:
		return StatusActive
	}
}

// Allows dice si el token tiene la capacidad sobre el entorno.
func (t Token) Allows(capability Capability, environment string) bool {
	return slices.Contains(t.Scopes, Scope(string(capability)+":"+environment))
}

// AllowsProject dice si el token tiene la capacidad sobre un proyecto: el
// scope de su entorno y, si el token tiene lista, el proyecto en ella.
func (t Token) AllowsProject(capability Capability, environment string, projectID uuid.UUID) bool {
	if !t.Allows(capability, environment) {
		return false
	}
	return t.AllProjects() || slices.Contains(t.ProjectIDs, projectID)
}

// AllProjects dice si el token alcanza a todos los proyectos de sus scopes.
func (t Token) AllProjects() bool {
	return len(t.ProjectIDs) == 0
}

// CanRead dice si el token tiene algún permiso de lectura. Sin ninguno no ve
// servidores (ADR-025).
func (t Token) CanRead() bool {
	return slices.ContainsFunc(t.Scopes, func(s Scope) bool { return s.Capability() == CapRead })
}

// Service crea, verifica y revoca tokens.
type Service struct {
	q   *store.Queries
	log *slog.Logger
	now func() time.Time
}

// NewService arma el servicio sobre la base.
func NewService(db store.DBTX, log *slog.Logger) *Service {
	return &Service{q: store.New(db), log: log, now: time.Now}
}

// NewToken es el pedido de un token nuevo.
type NewToken struct {
	Name       string
	Scopes     []string
	ProjectIDs []uuid.UUID
	// TTL es cuánto dura; cero es DefaultTTL.
	TTL time.Duration
	// CreatedBy es la persona que lo pide, o nil desde la línea de comandos.
	CreatedBy     *uuid.UUID
	CreatedByName string
}

// Create valida el pedido, guarda el token y devuelve su valor en claro, que
// no se puede volver a obtener.
func (s *Service) Create(ctx context.Context, req NewToken) (auth.Secret, Token, error) {
	name, err := normalizeName(req.Name)
	if err != nil {
		return auth.Secret{}, Token{}, err
	}
	scopes, err := ParseScopes(req.Scopes)
	if err != nil {
		return auth.Secret{}, Token{}, err
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl < 0 || ttl > MaxTTL {
		return auth.Secret{}, Token{}, fmt.Errorf("%w: el vencimiento tiene que ser de 90 días como mucho", ErrInvalid)
	}
	projectIDs, err := s.checkProjects(ctx, req.ProjectIDs)
	if err != nil {
		return auth.Secret{}, Token{}, err
	}

	secret, err := auth.NewMCPToken()
	if err != nil {
		return auth.Secret{}, Token{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return auth.Secret{}, Token{}, fmt.Errorf("generar el ID: %w", err)
	}
	now := s.now().UTC()
	row, err := s.q.CreateAPIToken(ctx, store.CreateAPITokenParams{
		ID:            id,
		Name:          name,
		Prefix:        secret.Reveal()[:PrefixLen],
		TokenHash:     secret.Hash(),
		Scopes:        scopeStrings(scopes),
		ProjectIds:    projectIDs,
		CreatedBy:     req.CreatedBy,
		CreatedByName: req.CreatedByName,
		CreatedAt:     now,
		ExpiresAt:     now.Add(ttl),
	})
	if err != nil {
		return auth.Secret{}, Token{}, fmt.Errorf("guardar el token: %w", err)
	}
	return secret, fromRow(row), nil
}

// checkProjects devuelve la lista sin repetidos y falla si alguno no existe:
// un ID equivocado no daría acceso a nada, pero el token no serviría.
func (s *Service) checkProjects(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	unique := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if !slices.Contains(unique, id) {
			unique = append(unique, id)
		}
	}
	if len(unique) > MaxProjects {
		return nil, fmt.Errorf("%w: la lista tiene más de %d proyectos", ErrInvalid, MaxProjects)
	}
	if len(unique) == 0 {
		return unique, nil
	}
	existing, err := s.q.ExistingProjectIDs(ctx, unique)
	if err != nil {
		return nil, fmt.Errorf("buscar los proyectos: %w", err)
	}
	for _, id := range unique {
		if !slices.Contains(existing, id) {
			return nil, fmt.Errorf("%w: el proyecto %s no existe", ErrInvalid, id)
		}
	}
	return unique, nil
}

// Authenticate devuelve el token de un valor si sigue vivo, y anota desde qué
// IP se usó.
func (s *Service) Authenticate(ctx context.Context, secret auth.Secret, ip string) (Token, error) {
	if len(secret.Reveal()) != tokenLen || !secret.HasPrefix(auth.MCPTokenPrefix) {
		return Token{}, ErrUnauthenticated
	}
	now := s.now().UTC()
	row, err := s.q.GetActiveAPIToken(ctx, store.GetActiveAPITokenParams{
		TokenHash: secret.Hash(),
		Now:       now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrUnauthenticated
	}
	if err != nil {
		return Token{}, fmt.Errorf("buscar el token: %w", err)
	}

	ip = truncate(ip, maxIPLen)
	if _, err := s.q.TouchAPIToken(ctx, store.TouchAPITokenParams{Now: now, Ip: ip, ID: row.ID}); err != nil {
		// No poder anotar el uso no invalida el token: el panel muestra un
		// último uso más viejo.
		s.log.WarnContext(ctx, "no se pudo anotar el uso de un token", "token", row.Prefix, "error", err)
	}
	return fromRow(row), nil
}

// List devuelve todos los tokens, los más nuevos primero.
func (s *Service) List(ctx context.Context) ([]Token, error) {
	rows, err := s.q.ListAPITokens(ctx)
	if err != nil {
		return nil, fmt.Errorf("listar los tokens: %w", err)
	}
	out := make([]Token, len(rows))
	for i, row := range rows {
		out[i] = fromRow(row)
	}
	return out, nil
}

// Get devuelve un token por su ID.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Token, error) {
	row, err := s.q.GetAPIToken(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	if err != nil {
		return Token{}, fmt.Errorf("buscar el token: %w", err)
	}
	return fromRow(row), nil
}

// Revoke revoca un token. Deja de servir en el próximo pedido.
func (s *Service) Revoke(ctx context.Context, id uuid.UUID, by string) (Token, error) {
	row, err := s.q.RevokeAPIToken(ctx, store.RevokeAPITokenParams{
		Now:           s.now().UTC(),
		RevokedByName: by,
		ID:            id,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// O no existe, o ya estaba revocado: se pregunta para decir cuál.
		if _, getErr := s.Get(ctx, id); getErr != nil {
			return Token{}, getErr
		}
		return Token{}, ErrAlreadyRevoked
	}
	if err != nil {
		return Token{}, fmt.Errorf("revocar el token: %w", err)
	}
	return fromRow(row), nil
}

// RevokeAll revoca todos los tokens que siguen sin revocar y dice cuántos.
func (s *Service) RevokeAll(ctx context.Context, by string) (int64, error) {
	n, err := s.q.RevokeAllAPITokens(ctx, store.RevokeAllAPITokensParams{
		Now:           s.now().UTC(),
		RevokedByName: by,
	})
	if err != nil {
		return 0, fmt.Errorf("revocar los tokens: %w", err)
	}
	return n, nil
}

// normalizeName recorta los espacios y valida el nombre: es lo que la
// auditoría muestra como actor.
func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("%w: falta el nombre", ErrInvalid)
	}
	if utf8.RuneCountInString(name) > MaxNameLen {
		return "", fmt.Errorf("%w: el nombre tiene más de %d caracteres", ErrInvalid, MaxNameLen)
	}
	if !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return "", fmt.Errorf("%w: el nombre tiene caracteres de control", ErrInvalid)
	}
	return name, nil
}

func fromRow(row store.ApiToken) Token {
	scopes := make([]Scope, len(row.Scopes))
	for i, scope := range row.Scopes {
		scopes[i] = Scope(scope)
	}
	t := Token{
		ID:            row.ID,
		Name:          row.Name,
		Prefix:        row.Prefix,
		Scopes:        scopes,
		ProjectIDs:    row.ProjectIds,
		CreatedByName: row.CreatedByName,
		CreatedAt:     row.CreatedAt,
		ExpiresAt:     row.ExpiresAt,
		RevokedAt:     row.RevokedAt,
		LastUsedAt:    row.LastUsedAt,
	}
	if row.RevokedByName != nil {
		t.RevokedByName = *row.RevokedByName
	}
	if row.LastUsedIp != nil {
		t.LastUsedIP = *row.LastUsedIp
	}
	return t
}

func truncate(v string, limit int) string {
	if len(v) <= limit {
		return v
	}
	return v[:limit]
}
