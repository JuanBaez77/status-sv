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
	finished := now.Add(time.Minute)
	d, err := apply(4, "succeeded", &finished, f.serverID)
	if err != nil || d.Status != "succeeded" || d.FinishedAt == nil {
		t.Fatalf("final = %+v, %v", d, err)
	}
	if seq, err := f.q.DeploySeq(ctx, store.DeploySeqParams{ID: id, ServerID: f.serverID}); err != nil || seq != 4 {
		t.Errorf("DeploySeq = %d, %v", seq, err)
	}
	// Un final sin fecha de fin lo rechaza la base.
	if _, err := apply(5, "failed", nil, f.serverID); err == nil {
		t.Error("se aceptó un deploy terminado sin fin")
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
