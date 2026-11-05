//go:build integration

package approvals

import (
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

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
	pool     *pgxpool.Pool
	q        *store.Queries
	s        *Service
	clock    *clock
	juan     Decider
	serverID uuid.UUID
	shop     uuid.UUID
	quoter   uuid.UUID
	tokenID  uuid.UUID
}

func setup(t *testing.T) fixture {
	t.Helper()
	pool := testdb.Setup(t)
	testdb.Truncate(t, pool)
	ctx := t.Context()
	q := store.New(pool)
	c := &clock{now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}

	user, err := q.CreateUser(ctx, store.CreateUserParams{
		ID: uuid.Must(uuid.NewV7()), Username: "juan",
		PasswordHash: "$argon2id$v=19$m=65536,t=3,p=1$c2Fs$aGFzaA",
	})
	if err != nil {
		t.Fatalf("crear el usuario: %v", err)
	}
	serverID := uuid.Must(uuid.NewV7())
	if _, err := q.CreateServer(ctx, store.CreateServerParams{
		ID: serverID, Name: "vps-aprobaciones", Hostname: "vps", Labels: []byte(`{}`), AgentVersion: "0.1.0",
	}); err != nil {
		t.Fatalf("crear el servidor: %v", err)
	}
	project := func(name string) uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		if err := q.UpsertProject(ctx, store.UpsertProjectParams{
			ID: id, ServerID: serverID, Name: name, Environment: "production", Repo: "github.com/a/" + name,
			Branch: "main", Runtime: "docker", Workdir: "/srv/" + name, ComposeFile: "compose.yml", ComposeProject: name,
			Units: []string{}, SeenAt: time.Now(),
		}); err != nil {
			t.Fatalf("crear el proyecto: %v", err)
		}
		return id
	}

	f := fixture{
		pool: pool, q: q, s: NewService(pool), clock: c,
		juan:     Decider{UserID: user.ID, Name: "juan"},
		serverID: serverID, shop: project("tienda"), quoter: project("cotizador"),
	}
	f.s.now = c.Now
	f.tokenID = f.token(t, "claude-code", c.now.Add(30*24*time.Hour))
	return f
}

// token guarda un token del MCP con su hash, sin pasar por el paquete tokens.
func (f fixture) token(t *testing.T, name string, expires time.Time) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	hash := sha256.Sum256([]byte(id.String()))
	if _, err := f.q.CreateAPIToken(t.Context(), store.CreateAPITokenParams{
		ID: id, Name: name, Prefix: "svmcp_AbCdEf", TokenHash: hash[:], Scopes: []string{"read:production"},
		ProjectIds: []uuid.UUID{}, CreatedByName: "juan", CreatedAt: f.clock.Now(), ExpiresAt: expires,
	}); err != nil {
		t.Fatalf("crear el token: %v", err)
	}
	return id
}

func (f fixture) request(op Operation, project uuid.UUID, name string) Request {
	r := Request{
		Operation: op, ServerID: f.serverID, ProjectID: project, ProjectName: name, Environment: "production",
		TokenID: f.tokenID, TokenName: "claude-code", TokenPrefix: "svmcp_AbCdEf", IP: "203.0.113.7",
		Reason: "arregla el total del PDF",
	}
	if op == OpDeploy {
		r.Commit = commit
	}
	return r
}

func TestCreateKeepsTheRequestPending(t *testing.T) {
	f := setup(t)
	a, err := f.s.Create(t.Context(), f.request(OpDeploy, f.shop, "tienda"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusPending || a.Operation != OpDeploy || a.Commit != commit || a.ServerName != "vps-aprobaciones" ||
		a.ProjectID == nil || *a.ProjectID != f.shop || a.RequestedByName != "claude-code" ||
		a.RequestedByToken == nil || *a.RequestedByToken != f.tokenID || a.Reason != "arregla el total del PDF" {
		t.Errorf("pedido = %+v", a)
	}
	if !a.ExpiresAt.Equal(f.clock.Now().Add(TTL)) || a.DecidedAt != nil || a.DeployID != nil {
		t.Errorf("vence %v, decidido %v, deploy %v", a.ExpiresAt, a.DecidedAt, a.DeployID)
	}
}

func TestOnePendingPerProject(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	first, err := f.s.Create(ctx, f.request(OpDeploy, f.shop, "tienda"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Create(ctx, f.request(OpRestart, f.shop, "tienda"))
	var pending *PendingExistsError
	if !errors.As(err, &pending) || pending.ID != first.ID {
		t.Fatalf("segundo pedido sobre tienda: err = %v", err)
	}
	if _, err := f.s.Create(ctx, f.request(OpRollback, f.quoter, "cotizador")); err != nil {
		t.Errorf("un pedido sobre otro proyecto: %v", err)
	}

	// Decidido el primero, se puede pedir de nuevo.
	if _, err := f.s.Reject(ctx, first.ID, f.juan, "no"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Create(ctx, f.request(OpRestart, f.shop, "tienda")); err != nil {
		t.Errorf("después de rechazar el primero: %v", err)
	}
}

func TestApproveAndRejectDecideOnce(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	a, err := f.s.Create(ctx, f.request(OpDeploy, f.shop, "tienda"))
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(5 * time.Minute)
	approved, err := f.s.Approve(ctx, a.ID, f.juan)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != StatusApproved || approved.DecidedByName != "juan" || approved.DecidedAt == nil ||
		!approved.DecidedAt.Equal(f.clock.Now()) {
		t.Errorf("aprobado = %+v", approved)
	}
	if again, err := f.s.Approve(ctx, a.ID, f.juan); !errors.Is(err, ErrNotPending) || again.Status != StatusApproved {
		t.Errorf("aprobar dos veces: %v (%s)", err, again.Status)
	}
	if _, err := f.s.Reject(ctx, a.ID, f.juan, "tarde"); !errors.Is(err, ErrNotPending) {
		t.Errorf("rechazar uno aprobado: %v", err)
	}

	b, err := f.s.Create(ctx, f.request(OpRestart, f.quoter, "cotizador"))
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := f.s.Reject(ctx, b.ID, f.juan, "  no es el momento\x00 ")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != StatusRejected || rejected.DecisionNote != "no es el momento" || rejected.DecidedByName != "juan" {
		t.Errorf("rechazado = %+v", rejected)
	}

	if _, err := f.s.Approve(ctx, uuid.New(), f.juan); !errors.Is(err, ErrNotFound) {
		t.Errorf("aprobar uno que no existe: %v", err)
	}
	if _, err := f.s.Approve(ctx, a.ID, Decider{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("aprobar sin persona: %v", err)
	}
}

func TestAnExpiredRequestCannotBeApproved(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	a, err := f.s.Create(ctx, f.request(OpDeploy, f.shop, "tienda"))
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(TTL)
	// Vencido pero sin barrer todavía: igual no se aprueba.
	if got, err := f.s.Approve(ctx, a.ID, f.juan); !errors.Is(err, ErrNotPending) || got.Status != StatusPending {
		t.Fatalf("aprobar uno vencido: %v (%s)", err, got.Status)
	}

	swept, err := f.s.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(swept.Expired) != 1 || swept.Expired[0].ID != a.ID || swept.Expired[0].Status != StatusExpired ||
		swept.Expired[0].DecidedByName != "" || len(swept.Cancelled) != 0 {
		t.Errorf("barrido = %+v", swept)
	}
	// Barrer de nuevo no cambia nada.
	if again, err := f.s.Sweep(ctx); err != nil || len(again.Expired)+len(again.Cancelled) != 0 {
		t.Errorf("segundo barrido = %+v, %v", again, err)
	}
}

func TestSweepCancelsRequestsOfDeadTokensAndMissingProjects(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	revocable := f.token(t, "revocable", f.clock.Now().Add(time.Hour*24))
	shortLived := f.token(t, "corto", f.clock.Now().Add(10*time.Minute))
	other := uuid.Must(uuid.NewV7())
	if err := f.q.UpsertProject(ctx, store.UpsertProjectParams{
		ID: other, ServerID: f.serverID, Name: "viejo", Environment: "production", Repo: "github.com/a/viejo",
		Branch: "main", Runtime: "docker", Workdir: "/srv/viejo", ComposeFile: "compose.yml", ComposeProject: "viejo",
		Units: []string{}, SeenAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	byRevoked := f.request(OpRestart, f.shop, "tienda")
	byRevoked.TokenID, byRevoked.TokenName = revocable, "revocable"
	r1, err := f.s.Create(ctx, byRevoked)
	if err != nil {
		t.Fatal(err)
	}
	byExpired := f.request(OpRollback, f.quoter, "cotizador")
	byExpired.TokenID, byExpired.TokenName = shortLived, "corto"
	r2, err := f.s.Create(ctx, byExpired)
	if err != nil {
		t.Fatal(err)
	}
	r3, err := f.s.Create(ctx, f.request(OpBootstrap, other, "viejo"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.q.RevokeAPIToken(ctx, store.RevokeAPITokenParams{Now: f.clock.Now(), RevokedByName: "juan", ID: revocable}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.DeleteProjectsExcept(ctx, store.DeleteProjectsExceptParams{
		ServerID: f.serverID, Keep: []string{"tienda", "cotizador"},
	}); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(15 * time.Minute)

	swept, err := f.s.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	notes := map[uuid.UUID]string{}
	for _, a := range swept.Cancelled {
		if a.Status != StatusCancelled || a.DecidedByName != "" {
			t.Errorf("cancelado = %+v", a)
		}
		notes[a.ID] = a.DecisionNote
	}
	if len(swept.Expired) != 0 || len(notes) != 3 ||
		notes[r1.ID] != "el token que lo pidió fue revocado o venció" ||
		notes[r2.ID] != "el token que lo pidió fue revocado o venció" ||
		notes[r3.ID] != "el proyecto dejó de estar declarado" {
		t.Errorf("barrido = %+v", swept)
	}
}

func TestCancelAndSetDeploy(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	a, err := f.s.Create(ctx, f.request(OpDeploy, f.shop, "tienda"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetDeploy(ctx, a.ID, uuid.New()); err == nil {
		t.Error("un pedido pendiente no puede tener deploy")
	}
	if _, err := f.s.Approve(ctx, a.ID, f.juan); err != nil {
		t.Fatal(err)
	}
	deployID := uuid.Must(uuid.NewV7())
	if err := f.q.CreateDeploy(ctx, store.CreateDeployParams{
		ID: deployID, ServerID: f.serverID, ProjectID: &f.shop, ProjectName: "tienda", Environment: "production",
		Kind: "deploy", RequestedCommit: commit, RequestedBy: &f.juan.UserID, RequestedByName: "juan",
		RequestedAt: f.clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetDeploy(ctx, a.ID, deployID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetDeploy(ctx, a.ID, deployID); err == nil {
		t.Error("un pedido tiene un solo deploy")
	}
	got, err := f.s.Get(ctx, a.ID)
	if err != nil || got.DeployID == nil || *got.DeployID != deployID {
		t.Errorf("pedido = %+v, %v", got, err)
	}

	b, err := f.s.Create(ctx, f.request(OpRestart, f.quoter, "cotizador"))
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := f.s.Cancel(ctx, b.ID, "el agent dejó de declararlo")
	if err != nil || cancelled.Status != StatusCancelled || cancelled.DecisionNote != "el agent dejó de declararlo" {
		t.Errorf("cancelado = %+v, %v", cancelled, err)
	}
	if _, err := f.s.Cancel(ctx, b.ID, "otra vez"); !errors.Is(err, ErrNotPending) {
		t.Errorf("cancelar dos veces: %v", err)
	}
}

func TestListFiltersByStatus(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	a, err := f.s.Create(ctx, f.request(OpDeploy, f.shop, "tienda"))
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Minute)
	b, err := f.s.Create(ctx, f.request(OpRestart, f.quoter, "cotizador"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Reject(ctx, a.ID, f.juan, ""); err != nil {
		t.Fatal(err)
	}

	all, err := f.s.List(ctx, "", 0)
	if err != nil || len(all) != 2 || all[0].ID != b.ID || all[1].ID != a.ID {
		t.Errorf("todos = %+v, %v", all, err)
	}
	pending, err := f.s.List(ctx, StatusPending, 0)
	if err != nil || len(pending) != 1 || pending[0].ID != b.ID {
		t.Errorf("pendientes = %+v, %v", pending, err)
	}
	one, err := f.s.List(ctx, "", 1)
	if err != nil || len(one) != 1 {
		t.Errorf("uno = %+v, %v", one, err)
	}
	if _, err := f.s.List(ctx, "aprobadito", 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("estado desconocido: %v", err)
	}
}

// TestTheTableEnforcesTheRules prueba las restricciones de la migración 00014
// sin pasar por el paquete: son la última barrera.
func TestTheTableEnforcesTheRules(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	base := func() store.CreateApprovalParams {
		now := f.clock.Now()
		return store.CreateApprovalParams{
			ID: uuid.Must(uuid.NewV7()), Operation: "deploy", ServerID: f.serverID, ProjectID: &f.shop,
			ProjectName: "tienda", Environment: "production", CommitSha: commit, RequestedByToken: &f.tokenID,
			RequestedByName: "claude-code", RequestedByPrefix: "svmcp_AbCdEf", RequestedIp: "203.0.113.7",
			RequestedAt: now, ExpiresAt: now.Add(TTL),
		}
	}
	cases := map[string]func(*store.CreateApprovalParams){
		"operación libre":         func(p *store.CreateApprovalParams) { p.Operation = "exec" },
		"deploy sin commit":       func(p *store.CreateApprovalParams) { p.CommitSha = "" },
		"rollback con commit":     func(p *store.CreateApprovalParams) { p.Operation = "rollback" },
		"unidad fuera de restart": func(p *store.CreateApprovalParams) { p.Unit = "web" },
		"vence en más de una hora": func(p *store.CreateApprovalParams) {
			p.ExpiresAt = p.RequestedAt.Add(TTL + time.Second)
		},
		"motivo larguísimo": func(p *store.CreateApprovalParams) {
			p.Reason = strings.Repeat("a", MaxReasonLen+1)
		},
	}
	for name, change := range cases {
		p := base()
		change(&p)
		if err := f.q.CreateApproval(ctx, p); err == nil {
			t.Errorf("%s: la base lo aceptó", name)
		}
	}
	if err := f.q.CreateApproval(ctx, base()); err != nil {
		t.Errorf("uno válido: %v", err)
	}
}
