// Package enrollment maneja el alta de un agent: crear el token de un solo uso
// que se le entrega y, cuando el agent lo presenta, canjearlo por su credencial.
//
// Este paquete es el único que genera tokens de enrolamiento, así que la regla
// de que viven 15 minutos y se usan una sola vez se cumple en un solo lugar.
package enrollment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// ErrInvalidServerName lo devuelve CreateToken cuando el nombre no sirve.
var ErrInvalidServerName = errors.New("nombre de servidor inválido")

// serverNamePattern acota el nombre a algo que se pueda usar como identificador
// en una URL, en una unidad de systemd y en un directorio, sin sorpresas.
var serverNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// Service crea y canjea tokens de enrolamiento.
type Service struct {
	pool    *pgxpool.Pool
	queries *store.Queries
	ttl     time.Duration
	log     *slog.Logger
}

// NewService arma el servicio. El ttl es cuánto vive cada token.
func NewService(pool *pgxpool.Pool, ttl time.Duration, log *slog.Logger) *Service {
	return &Service{
		pool:    pool,
		queries: store.New(pool),
		ttl:     ttl,
		log:     log,
	}
}

// CreateToken genera un token de un solo uso para enrolar un servidor con ese
// nombre. Devuelve el secreto en claro, que es la única vez que existe: la base
// guarda solo su hash.
//
// createdBy queda en la auditoría: hoy es "cli", y cuando exista el panel será
// el usuario que lo pidió.
func (s *Service) CreateToken(ctx context.Context, serverName, createdBy string) (auth.Secret, store.EnrollmentToken, error) {
	if !serverNamePattern.MatchString(serverName) {
		return auth.Secret{}, store.EnrollmentToken{}, fmt.Errorf(
			"%w: %q; se permiten letras, números, punto, guion y guion bajo, hasta 64 caracteres",
			ErrInvalidServerName, serverName)
	}
	if createdBy == "" {
		return auth.Secret{}, store.EnrollmentToken{}, errors.New("falta quién pide el token")
	}

	secret, err := auth.NewEnrollmentToken()
	if err != nil {
		return auth.Secret{}, store.EnrollmentToken{}, fmt.Errorf("generar el token: %w", err)
	}

	id, err := uuid.NewV7()
	if err != nil {
		return auth.Secret{}, store.EnrollmentToken{}, fmt.Errorf("generar el ID del token: %w", err)
	}

	token, err := s.queries.CreateEnrollmentToken(ctx, store.CreateEnrollmentTokenParams{
		ID:         id,
		TokenHash:  secret.Hash(),
		ServerName: serverName,
		ExpiresAt:  time.Now().Add(s.ttl),
		CreatedBy:  createdBy,
	})
	if err != nil {
		return auth.Secret{}, store.EnrollmentToken{}, fmt.Errorf("guardar el token: %w", err)
	}

	s.log.InfoContext(ctx, "token de enrolamiento creado",
		"token_id", token.ID,
		"servidor", serverName,
		"creado_por", createdBy,
		"vence", token.ExpiresAt,
	)

	return secret, token, nil
}

// SweepExpired borra los tokens que vencieron sin usarse. No es urgente que
// corra: un token vencido ya no sirve aunque siga en la tabla.
func (s *Service) SweepExpired(ctx context.Context) (int64, error) {
	deleted, err := s.queries.DeleteExpiredEnrollmentTokens(ctx)
	if err != nil {
		return 0, fmt.Errorf("borrar tokens vencidos: %w", err)
	}
	if deleted > 0 {
		s.log.InfoContext(ctx, "tokens de enrolamiento vencidos borrados", "cantidad", deleted)
	}
	return deleted, nil
}

// ValidServerName dice si un nombre de servidor es aceptable. Lo usa el
// subcomando de la CLI para fallar antes de tocar la base.
func ValidServerName(name string) bool {
	return serverNamePattern.MatchString(name)
}
