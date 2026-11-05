//go:build integration

package tokens

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/store"
	"github.com/JuanBaez77/status-sv/server/internal/testdb"
)

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

type fixture struct {
	pool      *pgxpool.Pool
	s         *Service
	clock     *clock
	userID    uuid.UUID
	projectID uuid.UUID
}

func setup(t *testing.T) fixture {
	t.Helper()
	pool := testdb.Setup(t)
	testdb.Truncate(t, pool)
	ctx := t.Context()
	q := store.New(pool)

	user, err := q.CreateUser(ctx, store.CreateUserParams{
		ID: uuid.Must(uuid.NewV7()), Username: "juan",
		PasswordHash: "$argon2id$v=19$m=65536,t=3,p=1$c2Fs$aGFzaA",
	})
	if err != nil {
		t.Fatalf("crear el usuario: %v", err)
	}
	serverID := uuid.Must(uuid.NewV7())
	if _, err := q.CreateServer(ctx, store.CreateServerParams{
		ID: serverID, Name: "vps-tokens", Hostname: "vps", Labels: []byte(`{}`), AgentVersion: "0.1.0",
	}); err != nil {
		t.Fatalf("crear el servidor: %v", err)
	}
	projectID := uuid.Must(uuid.NewV7())
	if err := q.UpsertProject(ctx, store.UpsertProjectParams{
		ID: projectID, ServerID: serverID, Name: "tienda", Environment: "production", Repo: "github.com/a/tienda",
		Branch: "main", Runtime: "docker", Workdir: "/srv/tienda", ComposeFile: "compose.yml", ComposeProject: "tienda",
		Units: []string{}, SeenAt: time.Now(),
	}); err != nil {
		t.Fatalf("crear el proyecto: %v", err)
	}

	c := &clock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	s := NewService(pool, slog.New(slog.DiscardHandler))
	s.now = c.Now
	return fixture{pool: pool, s: s, clock: c, userID: user.ID, projectID: projectID}
}

func (f fixture) create(t *testing.T, req NewToken) (auth.Secret, Token) {
	t.Helper()
	if req.CreatedByName == "" {
		req.CreatedBy = &f.userID
		req.CreatedByName = "juan"
	}
	secret, tok, err := f.s.Create(t.Context(), req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return secret, tok
}

func TestCreateKeepsOnlyTheHash(t *testing.T) {
	f := setup(t)
	secret, tok := f.create(t, NewToken{Name: " claude-code ", Scopes: []string{"logs:staging", "read:staging"}})

	value := secret.Reveal()
	if !strings.HasPrefix(value, auth.MCPTokenPrefix) || len(value) != tokenLen {
		t.Fatalf("token = %q", value)
	}
	if tok.Name != "claude-code" || tok.Prefix != value[:PrefixLen] {
		t.Errorf("nombre %q y prefijo %q", tok.Name, tok.Prefix)
	}
	if got := scopeStrings(tok.Scopes); strings.Join(got, ",") != "read:staging,logs:staging" {
		t.Errorf("scopes = %v", got)
	}
	if !tok.AllProjects() {
		t.Errorf("sin lista tiene que alcanzar a todos, y quedó %v", tok.ProjectIDs)
	}
	if want := f.clock.Now().Add(DefaultTTL); !tok.ExpiresAt.Equal(want) {
		t.Errorf("vence %v, se esperaba %v", tok.ExpiresAt, want)
	}

	var stored []byte
	var prefix string
	if err := f.pool.QueryRow(t.Context(),
		"SELECT token_hash, prefix FROM api_tokens WHERE id = $1", tok.ID).Scan(&stored, &prefix); err != nil {
		t.Fatalf("leer el token guardado: %v", err)
	}
	if !bytes.Equal(stored, auth.HashOf(value)) {
		t.Error("lo guardado no es el SHA-256 del token")
	}
	if strings.Contains(prefix, value[PrefixLen:]) {
		t.Error("el prefijo guardado tiene más del token de lo que debería")
	}
}

func TestCreateValidates(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	base := NewToken{Name: "ok", Scopes: []string{"read:staging"}, CreatedByName: "juan"}

	cases := map[string]func(r *NewToken){
		"sin nombre":           func(r *NewToken) { r.Name = " " },
		"sin scopes":           func(r *NewToken) { r.Scopes = nil },
		"scope inventado":      func(r *NewToken) { r.Scopes = []string{"exec:production"} },
		"más de 90 días":       func(r *NewToken) { r.TTL = MaxTTL + time.Second },
		"vencimiento negativo": func(r *NewToken) { r.TTL = -time.Hour },
		"proyecto que no está": func(r *NewToken) { r.ProjectIDs = []uuid.UUID{uuid.Must(uuid.NewV7())} },
		"demasiados proyectos": func(r *NewToken) {
			for range MaxProjects + 1 {
				r.ProjectIDs = append(r.ProjectIDs, uuid.Must(uuid.NewV7()))
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := base
			mutate(&req)
			if _, _, err := f.s.Create(ctx, req); !errors.Is(err, ErrInvalid) {
				t.Errorf("Create = %v, se esperaba ErrInvalid", err)
			}
		})
	}

	_, tok := f.create(t, NewToken{
		Name: "con lista", Scopes: []string{"read:production"}, TTL: MaxTTL,
		ProjectIDs: []uuid.UUID{f.projectID, f.projectID},
	})
	if len(tok.ProjectIDs) != 1 || tok.ProjectIDs[0] != f.projectID {
		t.Errorf("lista = %v, se esperaba el proyecto una vez", tok.ProjectIDs)
	}
	if !tok.AllowsProject(CapRead, EnvProduction, f.projectID) {
		t.Error("el token tiene que alcanzar al proyecto de su lista")
	}
}

func TestDatabaseRejectsWhatTheServiceWouldNot(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	q := store.New(f.pool)
	now := f.clock.Now()

	insert := func(scopes []string, expires time.Time) error {
		secret, err := auth.NewMCPToken()
		if err != nil {
			t.Fatal(err)
		}
		_, err = q.CreateAPIToken(ctx, store.CreateAPITokenParams{
			ID: uuid.Must(uuid.NewV7()), Name: "directo", Prefix: secret.Reveal()[:PrefixLen],
			TokenHash: secret.Hash(), Scopes: scopes, ProjectIds: []uuid.UUID{},
			CreatedByName: "juan", CreatedAt: now, ExpiresAt: expires,
		})
		return err
	}
	if err := insert([]string{"read:staging"}, now.Add(MaxTTL)); err != nil {
		t.Fatalf("un token válido tiene que entrar: %v", err)
	}
	if err := insert(scopeStrings(KnownScopes()), now.Add(time.Hour)); err != nil {
		t.Errorf("un token con todos los scopes, también los de escritura, tiene que entrar: %v", err)
	}
	for _, scope := range []string{"exec:production", "approve:production", "deploy:dev"} {
		if err := insert([]string{scope}, now.Add(time.Hour)); err == nil {
			t.Errorf("la base aceptó el scope %q, fuera de la lista", scope)
		}
	}
	if err := insert([]string{}, now.Add(time.Hour)); err == nil {
		t.Error("la base aceptó un token sin scopes")
	}
	if err := insert([]string{"read:staging"}, now.Add(MaxTTL+time.Second)); err == nil {
		t.Error("la base aceptó un vencimiento de más de 90 días")
	}
}

func TestAuthenticate(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	secret, created := f.create(t, NewToken{Name: "claude-code", Scopes: []string{"read:staging"}, TTL: time.Hour})

	tok, err := f.s.Authenticate(ctx, secret, "203.0.113.7")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if tok.ID != created.ID || tok.Name != "claude-code" {
		t.Errorf("token = %+v", tok)
	}
	again, err := f.s.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.LastUsedAt == nil || !again.LastUsedAt.Equal(f.clock.Now()) || again.LastUsedIP != "203.0.113.7" {
		t.Errorf("último uso = %v desde %q", again.LastUsedAt, again.LastUsedIP)
	}

	for name, value := range map[string]string{
		"vacío":            "",
		"otro prefijo":     "svses_" + secret.Reveal()[len(auth.MCPTokenPrefix):],
		"una letra menos":  secret.Reveal()[:tokenLen-1],
		"uno que no está":  mustToken(t),
		"un byte cambiado": flipLast(secret.Reveal()),
	} {
		if _, err := f.s.Authenticate(ctx, auth.ParseSecret(value), "203.0.113.7"); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: Authenticate = %v, se esperaba ErrUnauthenticated", name, err)
		}
	}

	f.clock.Advance(time.Hour)
	if _, err := f.s.Authenticate(ctx, secret, "203.0.113.7"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("vencido: Authenticate = %v, se esperaba ErrUnauthenticated", err)
	}
}

func TestTouchAtMostOncePerMinute(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	secret, created := f.create(t, NewToken{Name: "claude-code", Scopes: []string{"read:staging"}})
	first := f.clock.Now()

	if _, err := f.s.Authenticate(ctx, secret, "203.0.113.7"); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(30 * time.Second)
	if _, err := f.s.Authenticate(ctx, secret, "203.0.113.7"); err != nil {
		t.Fatal(err)
	}
	tok, _ := f.s.Get(ctx, created.ID)
	if !tok.LastUsedAt.Equal(first) {
		t.Errorf("a los 30 s desde la misma IP no se anota otra vez: último uso %v", tok.LastUsedAt)
	}

	if _, err := f.s.Authenticate(ctx, secret, "198.51.100.9"); err != nil {
		t.Fatal(err)
	}
	tok, _ = f.s.Get(ctx, created.ID)
	if tok.LastUsedIP != "198.51.100.9" {
		t.Errorf("una IP nueva se anota enseguida, y quedó %q", tok.LastUsedIP)
	}

	f.clock.Advance(time.Minute)
	if _, err := f.s.Authenticate(ctx, secret, "198.51.100.9"); err != nil {
		t.Fatal(err)
	}
	tok, _ = f.s.Get(ctx, created.ID)
	if !tok.LastUsedAt.Equal(f.clock.Now()) {
		t.Errorf("pasado el minuto se anota: último uso %v", tok.LastUsedAt)
	}
}

func TestRevoke(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	secret, created := f.create(t, NewToken{Name: "claude-code", Scopes: []string{"read:staging"}})
	other, _ := f.create(t, NewToken{Name: "otro", Scopes: []string{"logs:staging"}})

	revoked, err := f.s.Revoke(ctx, created.ID, "juan")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if revoked.Status(f.clock.Now()) != StatusRevoked || revoked.RevokedByName != "juan" {
		t.Errorf("revocado = %+v", revoked)
	}
	if _, err := f.s.Authenticate(ctx, secret, "203.0.113.7"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("revocado: Authenticate = %v, se esperaba ErrUnauthenticated", err)
	}
	if _, err := f.s.Revoke(ctx, created.ID, "juan"); !errors.Is(err, ErrAlreadyRevoked) {
		t.Errorf("revocar dos veces = %v, se esperaba ErrAlreadyRevoked", err)
	}
	if _, err := f.s.Revoke(ctx, uuid.Must(uuid.NewV7()), "juan"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revocar uno que no existe = %v, se esperaba ErrNotFound", err)
	}

	n, err := f.s.RevokeAll(ctx, "cli")
	if err != nil || n != 1 {
		t.Fatalf("RevokeAll = %d, %v; se esperaba 1 (el otro ya estaba revocado)", n, err)
	}
	if _, err := f.s.Authenticate(ctx, other, "203.0.113.7"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("después de RevokeAll: Authenticate = %v", err)
	}

	list, err := f.s.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %d tokens, %v", len(list), err)
	}
	if list[0].Name != "otro" {
		t.Errorf("la lista va de más nuevo a más viejo, y empieza con %q", list[0].Name)
	}
}

func mustToken(t *testing.T) string {
	t.Helper()
	secret, err := auth.NewMCPToken()
	if err != nil {
		t.Fatal(err)
	}
	return secret.Reveal()
}

func flipLast(value string) string {
	last := value[len(value)-1]
	replacement := byte('A')
	if last == 'A' {
		replacement = 'B'
	}
	return value[:len(value)-1] + string(replacement)
}
