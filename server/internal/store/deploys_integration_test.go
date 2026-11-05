//go:build integration

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// deployFixture es un servidor con un proyecto declarado y un usuario.
type deployFixture struct {
	q         *store.Queries
	serverID  uuid.UUID
	projectID uuid.UUID
	userID    uuid.UUID
}

func newDeployFixture(t *testing.T) deployFixture {
	t.Helper()
	q, _ := setup(t)
	ctx := t.Context()

	serverID := uuid.Must(uuid.NewV7())
	if _, err := q.CreateServer(ctx, store.CreateServerParams{
		ID: serverID, Name: "vps-deploys", Hostname: "vps", Labels: []byte(`{}`), AgentVersion: "0.1.0",
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
	user, err := q.CreateUser(ctx, store.CreateUserParams{ID: uuid.Must(uuid.NewV7()), Username: "juan", PasswordHash: "$argon2id$v=19$m=65536,t=3,p=1$c2Fs$aGFzaA"})
	if err != nil {
		t.Fatalf("crear el usuario: %v", err)
	}
	return deployFixture{q: q, serverID: serverID, projectID: projectID, userID: user.ID}
}

func (f deployFixture) create(t *testing.T, at time.Time) (uuid.UUID, error) {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	return id, f.q.CreateDeploy(t.Context(), store.CreateDeployParams{
		ID: id, ServerID: f.serverID, ProjectID: &f.projectID, ProjectName: "tienda", Environment: "production",
		Kind: "deploy", RequestedBy: &f.userID, RequestedByName: "juan", RequestedAt: at,
	})
}

func TestOnlyOneActiveDeployPerProject(t *testing.T) {
	f := newDeployFixture(t)
	now := time.Now().UTC()

	first, err := f.create(t, now)
	if err != nil {
		t.Fatalf("primer deploy: %v", err)
	}
	_, err = f.create(t, now)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != uniqueViolation {
		t.Fatalf("segundo deploy activo = %v, se esperaba una violación de unicidad", err)
	}

	// Terminado el primero, se puede pedir otro.
	if _, err := f.q.RejectDeploy(t.Context(), store.RejectDeployParams{ID: first, Code: "busy", Message: "ocupado", At: now}); err != nil {
		t.Fatalf("RejectDeploy: %v", err)
	}
	if _, err := f.create(t, now); err != nil {
		t.Fatalf("otro deploy después de terminar el primero: %v", err)
	}
}

func TestApplyDeployEventOnlyMovesForward(t *testing.T) {
	f := newDeployFixture(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	id, err := f.create(t, now)
	if err != nil {
		t.Fatal(err)
	}

	apply := func(seq int32, status string, finished *time.Time, serverID uuid.UUID) (store.Deploy, error) {
		return f.q.ApplyDeployEvent(ctx, store.ApplyDeployEventParams{
			ID: id, ServerID: serverID, Status: status, DeployedCommit: "2222222222222222222222222222222222222222",
			PreviousCommit: "1111111111111111111111111111111111111111", StartedAt: &now, FinishedAt: finished,
			Seq: seq, Steps: []byte(`[{"name":"preflight","status":"ok"}]`), UpdatedAt: now,
		})
	}

	if d, err := apply(3, "running", nil, f.serverID); err != nil || d.Status != "running" || d.Seq != 3 {
		t.Fatalf("seq 3 = %+v, %v", d, err)
	}
	// Una foto más vieja, o la misma otra vez, no se aplica.
	for _, seq := range []int32{2, 3} {
		if _, err := apply(seq, "running", nil, f.serverID); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("seq %d = %v, se esperaba que no se aplicara", seq, err)
		}
	}
	// Una foto del mismo deploy que llega por el canal de otro servidor, no.
	if _, err := apply(9, "succeeded", &now, uuid.Must(uuid.NewV7())); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("foto de otro servidor = %v", err)
	}
	// Un final sin fecha de fin lo rechaza la base.
	var pgErr *pgconn.PgError
	if _, err := apply(4, "failed", nil, f.serverID); !errors.As(err, &pgErr) {
		t.Errorf("deploy terminado sin fin = %v, se esperaba que la base lo rechazara", err)
	}
	finished := now.Add(time.Minute)
	d, err := apply(4, "succeeded", &finished, f.serverID)
	if err != nil || d.Status != "succeeded" || d.FinishedAt == nil {
		t.Fatalf("final = %+v, %v", d, err)
	}
	// Lo que el agent cerró no se reabre ni cambia de final, aunque la foto
	// traiga un número más alto.
	later := finished.Add(time.Minute)
	for _, c := range []struct {
		status   string
		finished *time.Time
	}{{"running", nil}, {"rolled_back", &later}} {
		if _, err := apply(9, c.status, c.finished, f.serverID); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("foto %s sobre un deploy cerrado = %v, se esperaba que no se aplicara", c.status, err)
		}
	}
	if seq, err := f.q.DeploySeq(ctx, store.DeploySeqParams{ID: id, ServerID: f.serverID}); err != nil || seq != 4 {
		t.Errorf("DeploySeq = %d, %v", seq, err)
	}
	if got, err := f.q.GetDeploy(ctx, id); err != nil || got.Status != "succeeded" {
		t.Errorf("el deploy quedó %q, %v", got.Status, err)
	}
}

// Un deploy que el control plane cerró sin recibir ninguna foto (el agent no
// contestó a tiempo) revive si el agent lo había empezado igual.
func TestApplyDeployEventRevivesAnExpiredDeploy(t *testing.T) {
	f := newDeployFixture(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	id, err := f.create(t, now.Add(-5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if expired, err := f.q.ExpirePendingDeploys(ctx, store.ExpirePendingDeploysParams{Now: now, Before: now.Add(-2 * time.Minute)}); err != nil || len(expired) != 1 {
		t.Fatalf("expirados = %+v, %v", expired, err)
	}
	d, err := f.q.ApplyDeployEvent(ctx, store.ApplyDeployEventParams{
		ID: id, ServerID: f.serverID, Status: "running", StartedAt: &now, Seq: 1, Steps: []byte(`[]`), UpdatedAt: now,
	})
	if err != nil || d.Status != "running" || d.Seq != 1 {
		t.Fatalf("la primera foto de un deploy expirado = %+v, %v", d, err)
	}
}

func TestExpirePendingDeploys(t *testing.T) {
	f := newDeployFixture(t)
	now := time.Now().UTC()
	old, err := f.create(t, now.Add(-5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	expired, err := f.q.ExpirePendingDeploys(t.Context(), store.ExpirePendingDeploysParams{Now: now, Before: now.Add(-2 * time.Minute)})
	if err != nil || len(expired) != 1 || expired[0].ID != old || expired[0].Code != "no_answer" {
		t.Fatalf("expirados = %+v, %v", expired, err)
	}
	// El proyecto queda libre para otro deploy.
	if _, err := f.create(t, now); err != nil {
		t.Fatalf("deploy después de expirar el anterior: %v", err)
	}
	again, err := f.q.ExpirePendingDeploys(t.Context(), store.ExpirePendingDeploysParams{Now: now, Before: now.Add(-2 * time.Minute)})
	if err != nil || len(again) != 0 {
		t.Fatalf("expiró uno reciente: %+v, %v", again, err)
	}
}

// Si el proyecto deja de estar declarado, la historia queda.
func TestDeployHistorySurvivesTheProject(t *testing.T) {
	f := newDeployFixture(t)
	ctx := t.Context()
	now := time.Now().UTC()
	id, err := f.create(t, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.DeleteProjectsExcept(ctx, store.DeleteProjectsExceptParams{ServerID: f.serverID, Keep: []string{}}); err != nil {
		t.Fatal(err)
	}
	d, err := f.q.GetDeploy(ctx, id)
	if err != nil || d.ProjectID != nil || d.ProjectName != "tienda" || d.ServerName != "vps-deploys" {
		t.Fatalf("deploy después de borrar el proyecto = %+v, %v", d, err)
	}
	history, err := f.q.ListProjectDeploys(ctx, store.ListProjectDeploysParams{ServerID: f.serverID, ProjectName: "tienda", MaxRows: 10})
	if err != nil || len(history) != 1 || history[0].ID != id {
		t.Fatalf("historia = %+v, %v", history, err)
	}
}

// Un reinicio (ADR-026) va en la misma historia: sin commit pedido ni
// anterior, y sin vuelta atrás.
func TestRestartsGoInTheSameHistory(t *testing.T) {
	f := newDeployFixture(t)
	ctx := t.Context()
	now := time.Now().UTC()
	restart := func(commit string) (uuid.UUID, error) {
		id := uuid.Must(uuid.NewV7())
		return id, f.q.CreateDeploy(ctx, store.CreateDeployParams{
			ID: id, ServerID: f.serverID, ProjectID: &f.projectID, ProjectName: "tienda", Environment: "production",
			Kind: "restart", RequestedCommit: commit, RequestedBy: &f.userID, RequestedByName: "juan", RequestedAt: now,
		})
	}
	if _, err := restart("4f1c9a2b7d3e4f1c9a2b7d3e4f1c9a2b7d3e4f1c"); err == nil {
		t.Error("la base aceptó un reinicio con commit pedido")
	}
	id, err := restart("")
	if err != nil {
		t.Fatalf("un reinicio: %v", err)
	}
	// Mientras corre, es el deploy activo del proyecto: no se pide otro.
	if _, err := f.create(t, now); err == nil {
		t.Error("se pidió un deploy mientras corría un reinicio")
	}

	finished := now.Add(time.Minute)
	steps := []byte(`[]`)
	apply := func(status, previous string, seq int32) error {
		_, err := f.q.ApplyDeployEvent(ctx, store.ApplyDeployEventParams{
			ID: id, ServerID: f.serverID, Status: status, DeployedCommit: "89abcdef0123456789abcdef0123456789abcdef",
			PreviousCommit: previous, Code: "unhealthy", StartedAt: &now, FinishedAt: &finished, Seq: seq,
			Steps: steps, UpdatedAt: finished,
		})
		return err
	}
	if err := apply("rolled_back", "", 1); err == nil {
		t.Error("la base aceptó un reinicio que vuelve atrás")
	}
	if err := apply("failed", "4f1c9a2b7d3e4f1c9a2b7d3e4f1c9a2b7d3e4f1c", 1); err == nil {
		t.Error("la base aceptó un reinicio con commit anterior")
	}
	if err := apply("failed", "", 1); err != nil {
		t.Errorf("un reinicio que no quedó bien: %v", err)
	}
}
