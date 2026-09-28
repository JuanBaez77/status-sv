//go:build integration

package users

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"

	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/store"
	"github.com/JuanBaez77/status-sv/server/internal/testdb"
)

const password = "una contraseña larga"

// clock es un reloj que los tests adelantan a mano.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func setup(t *testing.T) (*pgxpool.Pool, *Service, *clock) {
	t.Helper()
	pool := testdb.Setup(t)
	testdb.Truncate(t, pool)

	c := &clock{now: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)}
	key, err := auth.NewTOTPKey()
	if err != nil {
		t.Fatalf("NewTOTPKey: %v", err)
	}
	s := NewService(pool, key, slog.New(slog.DiscardHandler))
	s.now = c.Now
	return pool, s, c
}

func TestCreateUser(t *testing.T) {
	pool, s, _ := setup(t)

	user, err := s.CreateUser(t.Context(), "  Juan ", password)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if user.Username != "juan" {
		t.Errorf("username = %q, se esperaba en minúsculas", user.Username)
	}
	if !strings.HasPrefix(user.PasswordHash, "$argon2id$") || strings.Contains(user.PasswordHash, password) {
		t.Errorf("hash guardado = %q", user.PasswordHash)
	}

	if _, err := s.CreateUser(t.Context(), "JUAN", password); !errors.Is(err, ErrUserExists) {
		t.Errorf("alta repetida = %v, se esperaba ErrUserExists", err)
	}
	if _, err := s.CreateUser(t.Context(), "otro", "corta"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Errorf("contraseña corta = %v, se esperaba ErrWeakPassword", err)
	}
	if _, err := s.CreateUser(t.Context(), "no válido", password); !errors.Is(err, ErrInvalidUsername) {
		t.Errorf("nombre inválido = %v, se esperaba ErrInvalidUsername", err)
	}

	// La base exige el formato aunque la aplicación se equivoque.
	if _, err := store.New(pool).CreateUser(t.Context(), store.CreateUserParams{
		ID: user.ID, Username: "Mayúsculas", PasswordHash: user.PasswordHash,
	}); err == nil {
		t.Error("la base aceptó un nombre fuera de formato")
	}
}

func TestLoginAndAuthenticate(t *testing.T) {
	_, s, c := setup(t)
	if _, err := s.CreateUser(t.Context(), "juan", password); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	token, session, err := s.Login(t.Context(), "Juan", password, "203.0.113.7", "Firefox")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !token.HasPrefix(auth.SessionTokenPrefix) || session.Username != "juan" {
		t.Errorf("sesión = %+v", session)
	}
	// Sin TOTP todavía, la contraseña abre una sesión a medias para el alta, que
	// vence a los 5 minutos.
	if session.Stage != StageTOTPEnroll || session.Full() {
		t.Errorf("etapa = %q, se esperaba %q", session.Stage, StageTOTPEnroll)
	}
	if !session.ExpiresAt.Equal(c.Now().Add(HalfSessionLifetime)) {
		t.Errorf("vence %v, se esperaba a los 5 minutos", session.ExpiresAt)
	}

	got, err := s.Authenticate(t.Context(), token)
	if err != nil || got.ID != session.ID {
		t.Fatalf("Authenticate = %+v, %v", got, err)
	}

	// Lo que no es un token de sesión ni se busca.
	for _, bad := range []auth.Secret{{}, auth.ParseSecret("svagt_x"), auth.ParseSecret(auth.SessionTokenPrefix + "inventado")} {
		if _, err := s.Authenticate(t.Context(), bad); !errors.Is(err, ErrNoSession) {
			t.Errorf("Authenticate(%q) = %v, se esperaba ErrNoSession", bad.Reveal(), err)
		}
	}
}

func TestLoginFailuresLookTheSame(t *testing.T) {
	_, s, _ := setup(t)
	if _, err := s.CreateUser(t.Context(), "juan", password); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	for name, attempt := range map[string][2]string{
		"contraseña equivocada": {"juan", password + "!"},
		"usuario inexistente":   {"nadie", password},
		"nombre inválido":       {"no válido", password},
		"contraseña enorme":     {"juan", strings.Repeat("a", 10_000)},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := s.Login(t.Context(), attempt[0], attempt[1], "203.0.113.7", "Firefox")
			if !errors.Is(err, ErrInvalidCredentials) {
				t.Errorf("Login = %v, se esperaba ErrInvalidCredentials", err)
			}
		})
	}
}

func TestSessionExpiresWhenIdleOrTooOld(t *testing.T) {
	_, s, c := setup(t)
	if _, err := s.CreateUser(t.Context(), "juan", password); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Sin uso: a las 24 h se termina.
	var secret []byte
	idle := loginFull(t, s, c, &secret)
	// Con uso todos los días: sigue viva hasta los 7 días.
	active := loginFull(t, s, c, &secret)

	for day := 1; day <= 6; day++ {
		c.Advance(23 * time.Hour)
		if _, err := s.Authenticate(t.Context(), active); err != nil {
			t.Fatalf("día %d: la sesión en uso venció: %v", day, err)
		}
		if day == 2 {
			if _, err := s.Authenticate(t.Context(), idle); !errors.Is(err, ErrNoSession) {
				t.Errorf("la sesión sin uso sigue viva después de %v", 46*time.Hour)
			}
		}
	}

	// 6 × 23 h = 138 h; a las 168 h (7 días) vence aunque se use.
	c.Advance(23 * time.Hour)
	if _, err := s.Authenticate(t.Context(), active); err != nil {
		t.Fatalf("a las 161 h la sesión en uso venció: %v", err)
	}
	c.Advance(8 * time.Hour)
	if _, err := s.Authenticate(t.Context(), active); !errors.Is(err, ErrNoSession) {
		t.Errorf("la sesión sigue viva pasados los 7 días: %v", err)
	}

	n, err := s.DeleteExpired(t.Context())
	if err != nil || n != 2 {
		t.Errorf("DeleteExpired = %d, %v; se esperaban 2", n, err)
	}
}

func TestLogoutAndPasswordChangeCloseSessions(t *testing.T) {
	_, s, _ := setup(t)
	if _, err := s.CreateUser(t.Context(), "juan", password); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	first, _, err := s.Login(t.Context(), "juan", password, "", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	second, _, err := s.Login(t.Context(), "juan", password, "", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := s.Logout(t.Context(), first); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := s.Authenticate(t.Context(), first); !errors.Is(err, ErrNoSession) {
		t.Error("la sesión sigue viva después del logout")
	}
	if err := s.Logout(t.Context(), first); err != nil {
		t.Errorf("salir dos veces = %v, no debería ser error", err)
	}

	const newPassword = "otra contraseña larga"
	closed, err := s.SetPassword(t.Context(), "Juan", newPassword)
	if err != nil || closed != 1 {
		t.Fatalf("SetPassword = %d, %v; se esperaba cerrar 1 sesión", closed, err)
	}
	if _, err := s.Authenticate(t.Context(), second); !errors.Is(err, ErrNoSession) {
		t.Error("una sesión sobrevivió al cambio de contraseña")
	}
	if _, _, err := s.Login(t.Context(), "juan", password, "", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Error("la contraseña vieja sigue sirviendo")
	}
	if _, _, err := s.Login(t.Context(), "juan", newPassword, "", ""); err != nil {
		t.Errorf("la contraseña nueva no sirve: %v", err)
	}
	if _, err := s.SetPassword(t.Context(), "nadie", newPassword); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("SetPassword(nadie) = %v, se esperaba ErrUserNotFound", err)
	}
}

// oldHash es un hash válido de password con parámetros de argon2id más bajos
// que los actuales, como los que quedarían si algún día se suben.
func oldHash(password string) string {
	salt := []byte("sal-de-16-bytes!")
	key := argon2.IDKey([]byte(password), salt, 1, 8*1024, 1, 32)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, 8*1024, 1, 1,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// Un hash con parámetros viejos se actualiza solo al iniciar sesión, sin
// cambiar la fecha de la contraseña.
func TestLoginRehashesOldParameters(t *testing.T) {
	pool, s, _ := setup(t)
	user, err := s.CreateUser(t.Context(), "juan", password)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	q := store.New(pool)
	old := oldHash(password)
	if _, err := q.SetUserPassword(t.Context(), store.SetUserPasswordParams{ID: user.ID, PasswordHash: old, Changed: false}); err != nil {
		t.Fatalf("SetUserPassword: %v", err)
	}

	if _, _, err := s.Login(t.Context(), "juan", password, "", ""); err != nil {
		t.Fatalf("Login con hash viejo: %v", err)
	}
	after, err := q.GetUserByID(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if auth.NeedsRehash(after.PasswordHash) {
		t.Errorf("el hash no se actualizó: %q", after.PasswordHash)
	}
	if !after.PasswordChangedAt.Equal(user.PasswordChangedAt) {
		t.Error("actualizar el hash cambió la fecha de la contraseña")
	}
}
