//go:build integration

package proposals

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/store"
	"github.com/JuanBaez77/status-sv/server/internal/testdb"
)

type fixture struct {
	svc      *Service
	q        *store.Queries
	serverID uuid.UUID
	userID   uuid.UUID
	now      time.Time
}

func setup(t *testing.T) *fixture {
	t.Helper()
	pool := testdb.Setup(t)
	testdb.Truncate(t, pool)
	ctx := t.Context()

	f := &fixture{svc: NewService(pool), q: store.New(pool), now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	f.svc.now = func() time.Time { return f.now }
	f.serverID = f.server(t, "sv-gcg")
	user, err := f.q.CreateUser(ctx, store.CreateUserParams{ID: uuid.Must(uuid.NewV7()), Username: "juan", PasswordHash: "$argon2id$v=19$m=65536,t=3,p=1$c2Fs$aGFzaA"})
	if err != nil {
		t.Fatalf("crear el usuario: %v", err)
	}
	f.userID = user.ID
	return f
}

func (f *fixture) server(t *testing.T, name string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	if _, err := f.q.CreateServer(t.Context(), store.CreateServerParams{
		ID: id, Name: name, Hostname: name, Labels: []byte(`{}`), AgentVersion: "0.6.0",
	}); err != nil {
		t.Fatalf("crear el servidor: %v", err)
	}
	return id
}

func (f *fixture) actor() Actor {
	return Actor{UserID: &f.userID, Name: "juan", Type: "user"}
}

func (f *fixture) create(t *testing.T, req Request) store.ProjectProposal {
	t.Helper()
	p, err := f.svc.Create(t.Context(), f.serverID, req, f.actor())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return p
}

func (f *fixture) status(t *testing.T, id uuid.UUID) string {
	t.Helper()
	p, err := f.svc.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return p.Status
}

func TestCreate(t *testing.T) {
	f := setup(t)
	p := f.create(t, systemdRequest())
	data, _, _ := Build(systemdRequest())
	if p.Status != StatusPending || p.Spec != string(data) || p.SpecHash != proto.SpecHash(data) ||
		p.Runtime != "systemd" || p.Environment != "staging" || p.CreatedByName != "juan" || p.ActorType != "user" {
		t.Errorf("propuesta = %+v", p)
	}
	if !p.CreatedAt.Equal(f.now) || !p.ExpiresAt.Equal(f.now.Add(TTL)) || p.ClosedAt != nil {
		t.Errorf("fechas = %v %v %v", p.CreatedAt, p.ExpiresAt, p.ClosedAt)
	}
	got, err := f.svc.Get(t.Context(), p.ID)
	if err != nil || got.ServerName != "sv-gcg" {
		t.Errorf("Get = %+v %v", got, err)
	}

	// Una propuesta inválida no se guarda.
	bad := systemdRequest()
	bad.Site.Domain = "*.extendsystem.com"
	if _, err := f.svc.Create(t.Context(), f.serverID, bad, f.actor()); !errors.Is(err, ErrInvalid) {
		t.Errorf("Create inválida = %v", err)
	}
	if list, _ := f.svc.List(t.Context(), f.serverID); len(list) != 1 {
		t.Errorf("hay %d propuestas, se esperaba 1", len(list))
	}
}

// Una propuesta nueva con el mismo nombre reemplaza a la abierta; en otro
// servidor, conviven.
func TestCreateSupersedes(t *testing.T) {
	f := setup(t)
	first := f.create(t, systemdRequest())
	f.now = f.now.Add(time.Minute)
	second := f.create(t, systemdRequest())
	if got := f.status(t, first.ID); got != StatusSuperseded {
		t.Errorf("la primera quedó %s", got)
	}
	if second.Status != StatusPending {
		t.Errorf("la segunda quedó %s", second.Status)
	}
	other := f.server(t, "sv-otro")
	if _, err := f.svc.Create(t.Context(), other, systemdRequest(), f.actor()); err != nil {
		t.Fatalf("el mismo nombre en otro servidor: %v", err)
	}
	list, err := f.svc.List(t.Context(), f.serverID)
	if err != nil || len(list) != 2 || list[0].ID != second.ID {
		t.Errorf("List = %+v %v", list, err)
	}
}

// El índice único sostiene la regla aunque alguien se saltee el servicio.
func TestOneOpenProposalPerName(t *testing.T) {
	f := setup(t)
	p := f.create(t, systemdRequest())
	_, err := f.q.CreateProposal(t.Context(), store.CreateProposalParams{
		ID: uuid.Must(uuid.NewV7()), ServerID: f.serverID, Name: p.Name, Runtime: p.Runtime, Environment: p.Environment,
		Spec: p.Spec, SpecHash: p.SpecHash, CreatedByName: "juan", ActorType: "user", CreatedAt: f.now, ExpiresAt: f.now.Add(TTL),
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("segunda abierta = %v, se esperaba una violación de unicidad", err)
	}
}

func TestCreateRejectsDeclared(t *testing.T) {
	f := setup(t)
	if err := f.q.UpsertProject(t.Context(), store.UpsertProjectParams{
		ID: uuid.Must(uuid.NewV7()), ServerID: f.serverID, Name: "svdemo", Environment: "staging", Repo: "github.com/a/svdemo",
		Branch: "main", Runtime: "systemd", Workdir: "/srv/svdemo", Units: []string{"svdemo.service"}, SeenAt: f.now,
	}); err != nil {
		t.Fatalf("declarar: %v", err)
	}
	if _, err := f.svc.Create(t.Context(), f.serverID, systemdRequest(), f.actor()); !errors.Is(err, ErrDeclared) {
		t.Fatalf("Create de un declarado = %v", err)
	}
}

func TestWithdrawAndReject(t *testing.T) {
	f := setup(t)
	p := f.create(t, systemdRequest())
	closed, err := f.svc.Withdraw(t.Context(), p.ID)
	if err != nil || closed.Status != StatusWithdrawn || closed.ClosedAt == nil {
		t.Fatalf("Withdraw = %+v %v", closed, err)
	}
	if _, err := f.svc.Withdraw(t.Context(), p.ID); !errors.Is(err, ErrNotOpen) {
		t.Errorf("retirar dos veces = %v", err)
	}
	if _, err := f.svc.Rejected(t.Context(), p.ID, "llena"); !errors.Is(err, ErrNotOpen) {
		t.Errorf("rechazar una retirada = %v", err)
	}

	p = f.create(t, dockerRequest())
	closed, err = f.svc.Rejected(t.Context(), p.ID, "el agent ya tiene 10 propuestas")
	if err != nil || closed.Status != StatusRejected || closed.Message != "el agent ya tiene 10 propuestas" {
		t.Fatalf("Rejected = %+v %v", closed, err)
	}
}

func TestDelivered(t *testing.T) {
	f := setup(t)
	p := f.create(t, systemdRequest())
	// Un acuse de otro servidor no cuenta.
	if changed, err := f.svc.Delivered(t.Context(), f.server(t, "sv-otro"), p.ID); err != nil || changed {
		t.Fatalf("Delivered de otro servidor = %v %v", changed, err)
	}
	if changed, err := f.svc.Delivered(t.Context(), f.serverID, p.ID); err != nil || !changed {
		t.Fatalf("Delivered = %v %v", changed, err)
	}
	if changed, _ := f.svc.Delivered(t.Context(), f.serverID, p.ID); changed {
		t.Error("entregada dos veces")
	}
	got, _ := f.svc.Get(t.Context(), p.ID)
	if got.Status != StatusDelivered || got.DeliveredAt == nil || !got.DeliveredAt.Equal(f.now) {
		t.Errorf("propuesta = %+v", got)
	}
	// Entregada sigue abierta: se puede retirar.
	if _, err := f.svc.Withdraw(t.Context(), p.ID); err != nil {
		t.Errorf("retirar una entregada: %v", err)
	}
}

func TestExpire(t *testing.T) {
	f := setup(t)
	old := f.create(t, systemdRequest())
	f.now = f.now.Add(TTL / 2)
	young := f.create(t, dockerRequest())

	f.now = old.ExpiresAt.Add(time.Second)
	if open, _ := f.svc.Open(t.Context(), f.serverID); len(open) != 1 || open[0].ID != young.ID {
		t.Errorf("abiertas = %+v", open)
	}
	expired, err := f.svc.Expire(t.Context())
	if err != nil || len(expired) != 1 || expired[0].ID != old.ID || expired[0].Status != StatusExpired {
		t.Fatalf("Expire = %+v %v", expired, err)
	}
	if expired, _ := f.svc.Expire(t.Context()); len(expired) != 0 {
		t.Errorf("venció dos veces: %+v", expired)
	}
}

func state(projects []proto.Project, pending ...store.ProjectProposal) proto.ProjectsState {
	s := proto.ProjectsState{Source: proto.RuntimeSource{Status: proto.SourceOK}, Projects: projects}
	for _, p := range pending {
		s.Proposals = append(s.Proposals, proto.PendingProposal{ProposalID: p.ID, Name: p.Name, Hash: p.SpecHash})
	}
	return s
}

func declared(name, hash string) proto.Project {
	return proto.Project{Name: name, SpecHash: hash}
}

func TestReconcile(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	systemd := f.create(t, systemdRequest())
	docker := f.create(t, dockerRequest())

	// El agent recién conectado no tiene ninguna: hay que mandarle las dos.
	r, err := f.svc.Reconcile(ctx, f.serverID, state(nil))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(r.Send) != 2 || len(r.Accepted) != 0 || len(r.Withdraw) != 0 || r.Delivered != 0 {
		t.Fatalf("sin nada = %+v", r)
	}

	// Las tiene: quedan entregadas, una sola vez.
	r, _ = f.svc.Reconcile(ctx, f.serverID, state(nil, systemd, docker))
	if r.Delivered != 2 || len(r.Send) != 0 {
		t.Fatalf("con las dos = %+v", r)
	}
	if r, _ = f.svc.Reconcile(ctx, f.serverID, state(nil, systemd, docker)); r.Delivered != 0 {
		t.Errorf("entregadas dos veces = %+v", r)
	}

	// Una que el control plane no conoce (o ya cerró) se pide borrar.
	stray := store.ProjectProposal{ID: uuid.Must(uuid.NewV7()), Name: "vieja", SpecHash: systemd.SpecHash}
	if _, err := f.svc.Withdraw(ctx, docker.ID); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	r, _ = f.svc.Reconcile(ctx, f.serverID, state(nil, systemd, docker, stray))
	if len(r.Withdraw) != 2 || r.Withdraw[0].ProposalID != docker.ID || r.Withdraw[1].ProposalID != stray.ID {
		t.Fatalf("a borrar = %+v", r.Withdraw)
	}

	// Una persona la aceptó: el agent informa el proyecto declarado igual.
	r, _ = f.svc.Reconcile(ctx, f.serverID, state([]proto.Project{declared("svdemo", systemd.SpecHash)}))
	if len(r.Accepted) != 1 || !r.Accepted[0].Same || r.Accepted[0].Proposal.Status != StatusAccepted {
		t.Fatalf("aceptadas = %+v", r.Accepted)
	}
	if got, _ := f.svc.Get(ctx, systemd.ID); got.Message != "declarada igual a la propuesta" {
		t.Errorf("mensaje = %q", got.Message)
	}
	// La segunda vez ya no está abierta: no se anota de nuevo.
	if r, _ = f.svc.Reconcile(ctx, f.serverID, state([]proto.Project{declared("svdemo", systemd.SpecHash)})); len(r.Accepted) != 0 {
		t.Errorf("aceptada dos veces = %+v", r.Accepted)
	}
}

// Declarado distinto de lo propuesto (alguien lo editó antes de aceptar, o
// lo declaró a mano) también cierra la propuesta, y lo dice.
func TestReconcileDeclaredDifferently(t *testing.T) {
	f := setup(t)
	p := f.create(t, dockerRequest())
	r, err := f.svc.Reconcile(t.Context(), f.serverID, state([]proto.Project{declared("tienda", proto.SpecHash([]byte("otra cosa")))}))
	if err != nil || len(r.Accepted) != 1 || r.Accepted[0].Same {
		t.Fatalf("Reconcile = %+v %v", r, err)
	}
	if got, _ := f.svc.Get(t.Context(), p.ID); got.Status != StatusAccepted || got.Message != "declarada distinta de la propuesta" {
		t.Errorf("propuesta = %+v", got)
	}
}

// Si el agent no pudo leer sus proyectos, una lista vacía no dice nada: no
// se acepta ni se manda nada por eso.
func TestReconcileIgnoresProjectsOnFailedRead(t *testing.T) {
	f := setup(t)
	f.create(t, dockerRequest())
	s := state([]proto.Project{declared("tienda", "")})
	s.Source = proto.RuntimeSource{Status: proto.SourceError, Detail: "permiso denegado"}
	r, err := f.svc.Reconcile(t.Context(), f.serverID, s)
	if err != nil || len(r.Accepted) != 0 || len(r.Send) != 1 {
		t.Fatalf("Reconcile = %+v %v", r, err)
	}
}

// Las propuestas de otro servidor no se mezclan: ni se mandan, ni se dan por
// conocidas.
func TestReconcileIsPerServer(t *testing.T) {
	f := setup(t)
	mine := f.create(t, dockerRequest())
	other := f.server(t, "sv-otro")
	theirs, err := f.svc.Create(t.Context(), other, systemdRequest(), f.actor())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	r, err := f.svc.Reconcile(t.Context(), f.serverID, state(nil, theirs))
	if err != nil || len(r.Send) != 1 || r.Send[0].ID != mine.ID || len(r.Withdraw) != 1 || r.Withdraw[0].ProposalID != theirs.ID {
		t.Fatalf("Reconcile = %+v %v", r, err)
	}
	// Ni un proyecto declarado acá acepta la de otro servidor.
	if r, _ = f.svc.Reconcile(t.Context(), f.serverID, state([]proto.Project{declared("svdemo", theirs.SpecHash)})); len(r.Accepted) != 0 {
		t.Errorf("aceptó la de otro servidor: %+v", r.Accepted)
	}
	if got := f.status(t, theirs.ID); got != StatusPending {
		t.Errorf("la de otro servidor quedó %s", got)
	}
}
