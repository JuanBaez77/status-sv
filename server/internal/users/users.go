// Package users maneja las personas que usan el panel: su alta, su contraseña y
// sus sesiones (ADR-010).
//
// Las contraseñas se guardan con argon2id y las sesiones como el SHA-256 de un
// token al azar, que viaja en una cookie. Nada de lo que se guarda sirve para
// iniciar sesión si se filtra la base.
package users

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// Plazos de una sesión, por decisión de Juan: vence a las 24 h sin uso o a los
// 7 días de abierta, lo que llegue primero.
const (
	IdleTimeout = 24 * time.Hour
	MaxLifetime = 7 * 24 * time.Hour
)

// touchEvery es cada cuánto se anota el uso de una sesión. Escribir en cada
// pedido sería una escritura por pedido; con un minuto de resolución el plazo
// sin uso se corre como mucho eso.
const touchEvery = time.Minute

// maxConcurrentHashes acota cuántos argon2id corren a la vez. Cada uno usa
// 19 MiB: sin tope, una ráfaga de logins podría llevar el control plane más
// allá de su meta de memoria.
const maxConcurrentHashes = 2

// Largo máximo de lo que se guarda del cliente de una sesión. La IP y el agente
// de usuario son para que Juan reconozca sus sesiones, no para identificar a
// nadie: se cortan en vez de rechazar el pedido.
const (
	maxIPLen        = 64
	maxUserAgentLen = 512
)

var (
	// ErrInvalidCredentials es un login fallido. No dice si falló el usuario
	// o la contraseña, a propósito.
	ErrInvalidCredentials = errors.New("usuario o contraseña incorrectos")
	// ErrNoSession es un token que no corresponde a una sesión viva.
	ErrNoSession = errors.New("no hay una sesión válida")
	// ErrInvalidUsername es un nombre que no cumple el formato.
	ErrInvalidUsername = errors.New("nombre de usuario inválido: de 3 a 32 caracteres, minúsculas, números, punto, guion o guion bajo, empezando por letra o número")
	// ErrUserExists es un alta con un nombre que ya está en uso.
	ErrUserExists = errors.New("ya existe un usuario con ese nombre")
	// ErrUserNotFound es un usuario que no existe.
	ErrUserNotFound = errors.New("no existe ese usuario")
)

// usernamePattern es el mismo que exige la migración 00003.
var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,31}$`)

// NormalizeUsername pasa un nombre a su forma guardada: sin espacios alrededor
// y en minúsculas.
func NormalizeUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

// ValidUsername dice si un nombre ya normalizado cumple el formato.
func ValidUsername(username string) bool {
	return usernamePattern.MatchString(username)
}

// Beginner abre transacciones. Lo cumple *pgxpool.Pool.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// DB es lo que el servicio necesita de la base: las consultas y transacciones.
type DB interface {
	store.DBTX
	Beginner
}

// Session es una sesión viva.
type Session struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Username  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Service maneja usuarios y sesiones.
type Service struct {
	db     DB
	q      *store.Queries
	log    *slog.Logger
	now    func() time.Time
	hashes chan struct{}
}

// NewService arma el servicio.
func NewService(db DB, log *slog.Logger) *Service {
	return &Service{
		db:     db,
		q:      store.New(db),
		log:    log,
		now:    time.Now,
		hashes: make(chan struct{}, maxConcurrentHashes),
	}
}

// CreateUser da de alta a una persona.
func (s *Service) CreateUser(ctx context.Context, username, password string) (store.User, error) {
	username = NormalizeUsername(username)
	if !ValidUsername(username) {
		return store.User{}, ErrInvalidUsername
	}
	if err := auth.ValidatePassword(password); err != nil {
		return store.User{}, err
	}

	hash, err := s.hash(ctx, password)
	if err != nil {
		return store.User{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return store.User{}, fmt.Errorf("generar el ID: %w", err)
	}

	user, err := s.q.CreateUser(ctx, store.CreateUserParams{ID: id, Username: username, PasswordHash: hash})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return store.User{}, ErrUserExists
		}
		return store.User{}, fmt.Errorf("guardar el usuario: %w", err)
	}
	return user, nil
}

// SetPassword cambia la contraseña de un usuario y cierra todas sus sesiones,
// en una transacción: si alguien tenía la contraseña vieja, pierde el acceso en
// el mismo momento.
func (s *Service) SetPassword(ctx context.Context, username, password string) (closedSessions int64, err error) {
	username = NormalizeUsername(username)
	if err := auth.ValidatePassword(password); err != nil {
		return 0, err
	}

	user, err := s.q.GetUserByUsername(ctx, username)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrUserNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("buscar el usuario: %w", err)
	}

	hash, err := s.hash(ctx, password)
	if err != nil {
		return 0, err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("abrir la transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	q := store.New(tx)
	if _, err := q.SetUserPassword(ctx, store.SetUserPasswordParams{ID: user.ID, PasswordHash: hash, Changed: true}); err != nil {
		return 0, fmt.Errorf("guardar la contraseña: %w", err)
	}
	closed, err := q.DeleteUserSessions(ctx, user.ID)
	if err != nil {
		return 0, fmt.Errorf("cerrar las sesiones: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("confirmar la transacción: %w", err)
	}
	return closed, nil
}

// Login verifica usuario y contraseña y abre una sesión. Devuelve el token, que
// es lo único que sirve para usarla y no vuelve a estar disponible.
//
// Un usuario que no existe tarda lo mismo que una contraseña equivocada, y los
// dos devuelven ErrInvalidCredentials: el login no dice qué nombres existen.
func (s *Service) Login(ctx context.Context, username, password, ip, userAgent string) (auth.Secret, Session, error) {
	username = NormalizeUsername(username)

	user, err := s.q.GetUserByUsername(ctx, username)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err := s.burn(ctx, password); err != nil {
			return auth.Secret{}, Session{}, err
		}
		return auth.Secret{}, Session{}, ErrInvalidCredentials
	case err != nil:
		return auth.Secret{}, Session{}, fmt.Errorf("buscar el usuario: %w", err)
	}

	ok, err := s.verify(ctx, password, user.PasswordHash)
	if err != nil {
		return auth.Secret{}, Session{}, err
	}
	if !ok {
		return auth.Secret{}, Session{}, ErrInvalidCredentials
	}

	// Con la contraseña correcta en la mano se aprovecha para actualizar un hash
	// hecho con parámetros viejos. Si falla, el login sigue: se reintenta la
	// próxima vez.
	if auth.NeedsRehash(user.PasswordHash) {
		s.rehash(ctx, user.ID, password)
	}

	return s.openSession(ctx, user, ip, userAgent)
}

func (s *Service) openSession(ctx context.Context, user store.User, ip, userAgent string) (auth.Secret, Session, error) {
	token, err := auth.NewSessionToken()
	if err != nil {
		return auth.Secret{}, Session{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return auth.Secret{}, Session{}, fmt.Errorf("generar el ID: %w", err)
	}

	now := s.now().UTC()
	row, err := s.q.CreateSession(ctx, store.CreateSessionParams{
		ID:        id,
		UserID:    user.ID,
		TokenHash: token.Hash(),
		CreatedAt: now,
		ExpiresAt: now.Add(MaxLifetime),
		Ip:        truncate(ip, maxIPLen),
		UserAgent: truncate(userAgent, maxUserAgentLen),
	})
	if err != nil {
		return auth.Secret{}, Session{}, fmt.Errorf("guardar la sesión: %w", err)
	}
	return token, Session{
		ID:        row.ID,
		UserID:    user.ID,
		Username:  user.Username,
		CreatedAt: row.CreatedAt,
		ExpiresAt: row.ExpiresAt,
	}, nil
}

// Authenticate devuelve la sesión de un token, si sigue viva, y anota su uso.
func (s *Service) Authenticate(ctx context.Context, token auth.Secret) (Session, error) {
	if token.IsZero() || !token.HasPrefix(auth.SessionTokenPrefix) {
		return Session{}, ErrNoSession
	}

	now := s.now().UTC()
	row, err := s.q.GetActiveSession(ctx, store.GetActiveSessionParams{
		TokenHash:  token.Hash(),
		Now:        now,
		IdleCutoff: now.Add(-IdleTimeout),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, fmt.Errorf("buscar la sesión: %w", err)
	}

	if now.Sub(row.LastSeenAt) >= touchEvery {
		if err := s.q.TouchSession(ctx, store.TouchSessionParams{ID: row.ID, Now: now}); err != nil {
			// No poder anotar el uso no invalida la sesión: a lo sumo vence
			// un minuto antes.
			s.log.WarnContext(ctx, "no se pudo anotar el uso de una sesión", "error", err)
		}
	}

	return Session{
		ID:        row.ID,
		UserID:    row.UserID,
		Username:  row.Username,
		CreatedAt: row.CreatedAt,
		ExpiresAt: row.ExpiresAt,
	}, nil
}

// Logout cierra la sesión de un token. Cerrar una que ya no existe no es error.
func (s *Service) Logout(ctx context.Context, token auth.Secret) error {
	if token.IsZero() {
		return nil
	}
	if _, err := s.q.DeleteSessionByTokenHash(ctx, token.Hash()); err != nil {
		return fmt.Errorf("cerrar la sesión: %w", err)
	}
	return nil
}

// CleanupEvery es la frecuencia del barrido de sesiones vencidas.
const CleanupEvery = time.Hour

// DeleteExpired borra las sesiones vencidas y devuelve cuántas.
func (s *Service) DeleteExpired(ctx context.Context) (int64, error) {
	now := s.now().UTC()
	n, err := s.q.DeleteExpiredSessions(ctx, store.DeleteExpiredSessionsParams{
		Now:        now,
		IdleCutoff: now.Add(-IdleTimeout),
	})
	if err != nil {
		return 0, fmt.Errorf("borrar las sesiones vencidas: %w", err)
	}
	return n, nil
}

// RunCleanup barre las sesiones vencidas cada hora hasta que se cancela el
// contexto. Una sesión vencida ya no sirve aunque siga en la tabla: el barrido
// es para que la tabla no crezca, no para la seguridad.
func (s *Service) RunCleanup(ctx context.Context) {
	ticker := time.NewTicker(CleanupEvery)
	defer ticker.Stop()
	for {
		n, err := s.DeleteExpired(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			s.log.WarnContext(ctx, "falló el barrido de sesiones vencidas", "error", err)
		case n > 0:
			s.log.InfoContext(ctx, "sesiones vencidas borradas", "cantidad", n)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// hash, verify y burn corren argon2id con el tope de concurrencia.

func (s *Service) hash(ctx context.Context, password string) (string, error) {
	if err := s.acquire(ctx); err != nil {
		return "", err
	}
	defer s.release()
	return auth.HashPassword(password)
}

func (s *Service) verify(ctx context.Context, password, encoded string) (bool, error) {
	if err := s.acquire(ctx); err != nil {
		return false, err
	}
	defer s.release()
	ok, err := auth.VerifyPassword(password, encoded)
	if err != nil {
		return false, fmt.Errorf("verificar la contraseña: %w", err)
	}
	return ok, nil
}

func (s *Service) burn(ctx context.Context, password string) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	auth.BurnPasswordCheck(password)
	return nil
}

func (s *Service) rehash(ctx context.Context, userID uuid.UUID, password string) {
	hash, err := s.hash(ctx, password)
	if err == nil {
		_, err = s.q.SetUserPassword(ctx, store.SetUserPasswordParams{ID: userID, PasswordHash: hash, Changed: false})
	}
	if err != nil {
		s.log.WarnContext(ctx, "no se pudo actualizar el hash de una contraseña", "error", err)
	}
}

func (s *Service) acquire(ctx context.Context) error {
	select {
	case s.hashes <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) release() { <-s.hashes }

// truncate deja un texto que vino de un encabezado en UTF-8 válido (PostgreSQL
// rechaza lo demás, y también el carácter nulo) y lo corta a limit bytes sin
// partir un carácter.
func truncate(v string, limit int) string {
	v = strings.ReplaceAll(strings.ToValidUTF8(v, ""), "\x00", "")
	if len(v) <= limit {
		return v
	}
	v = v[:limit]
	// Lo único inválido puede ser el último carácter, partido por el corte.
	for len(v) > 0 && !utf8.ValidString(v) {
		v = v[:len(v)-1]
	}
	return v
}
