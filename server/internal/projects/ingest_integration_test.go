//go:build integration

package projects

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/store"
	"github.com/JuanBaez77/status-sv/server/internal/testdb"
)

var t0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func setup(t *testing.T) (*pgxpool.Pool, uuid.UUID, *Ingester) {
	t.Helper()
	pool := testdb.Setup(t)
	testdb.Truncate(t, pool)

	id := uuid.Must(uuid.NewV7())
	if _, err := store.New(pool).CreateServer(t.Context(), store.CreateServerParams{
		ID: id, Name: "vps-" + id.String()[:8], Hostname: "vps", Labels: []byte(`{}`), AgentVersion: "0.1.0",
	}); err != nil {
		t.Fatalf("crear el servidor: %v", err)
	}
	i := NewIngester(pool)
	i.now = func() time.Time { return t0 }
	return pool, id, i
}

func docker(name string) proto.Project {
	return proto.Project{
		Name: name, Environment: proto.EnvProduction, Repo: "github.com/x/" + name, Branch: "main",
		Runtime: proto.RuntimeDocker, Workdir: "/srv/" + name, ComposeFile: "docker-compose.yml", ComposeProject: name,
		Healthcheck: &proto.HealthcheckSpec{URL: "https://" + name + ".example.com/health", ExpectStatus: 200, TimeoutSeconds: 10},
		Health:      &proto.HealthResult{State: proto.HealthPassing, CheckedAt: t0.Add(-time.Minute), StatusCode: 200, LatencyMS: 80},
	}
}

func systemd(name string) proto.Project {
	return proto.Project{
		Name: name, Environment: proto.EnvStaging, Repo: "github.com/x/" + name, Branch: "main",
		Runtime: proto.RuntimeSystemd, Workdir: "/srv/" + name, Units: []string{"gunicorn@" + name + ".service"},
	}
}

func read(at time.Time, projects ...proto.Project) proto.ProjectsState {
	return proto.ProjectsState{TakenAt: at, Source: proto.RuntimeSource{Status: proto.SourceOK}, Projects: projects}
}

type row struct {
	id                uuid.UUID
	name, runtime     string
	units             []string
	healthState       string
	healthURL         string
	healthStatusCode  int
	healthCheckedAtOK bool
}

func stored(t *testing.T, pool *pgxpool.Pool, serverID uuid.UUID) []row {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT id, name, runtime, units, health_state, healthcheck_url, health_status_code, health_checked_at IS NOT NULL
		FROM projects WHERE server_id = $1 ORDER BY name`, serverID)
	if err != nil {
		t.Fatalf("leer los proyectos: %v", err)
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name, &r.runtime, &r.units, &r.healthState, &r.healthURL, &r.healthStatusCode, &r.healthCheckedAtOK); err != nil {
			t.Fatalf("leer una fila: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func invalidFiles(t *testing.T, pool *pgxpool.Pool, serverID uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT file FROM project_invalid_files WHERE server_id = $1 ORDER BY file`, serverID)
	if err != nil {
		t.Fatalf("leer los inválidos: %v", err)
	}
	defer rows.Close()
	var files []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

func mustIngest(t *testing.T, i *Ingester, serverID uuid.UUID, state proto.ProjectsState) bool {
	t.Helper()
	applied, err := i.Ingest(t.Context(), serverID, state)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	return applied
}

func TestIngestReplacesTheDeclaredProjects(t *testing.T) {
	pool, server, i := setup(t)

	state := read(t0, docker("cristales"), systemd("facturas"))
	state.Invalid = []proto.InvalidProject{{File: "roto.yaml", Error: "name: falta"}}
	if !mustIngest(t, i, server, state) {
		t.Fatal("la primera lectura no se aplicó")
	}
	first := stored(t, pool, server)
	if len(first) != 2 || first[0].name != "cristales" || first[1].name != "facturas" {
		t.Fatalf("proyectos = %+v", first)
	}
	if first[0].healthState != "passing" || first[0].healthStatusCode != 200 || !first[0].healthCheckedAtOK ||
		first[0].healthURL == "" {
		t.Errorf("healthcheck de cristales = %+v", first[0])
	}
	if !slices.Equal(first[1].units, []string{"gunicorn@facturas.service"}) || first[1].healthState != "" {
		t.Errorf("facturas = %+v", first[1])
	}
	if got := invalidFiles(t, pool, server); !slices.Equal(got, []string{"roto.yaml"}) {
		t.Errorf("inválidos = %v", got)
	}

	// Se arregla el archivo roto, desaparece facturas y cristales cambia: el
	// ID de cristales se conserva.
	changed := docker("cristales")
	changed.Branch = "release"
	changed.Health = nil
	mustIngest(t, i, server, read(t0.Add(time.Minute), changed, systemd("roto")))
	second := stored(t, pool, server)
	if len(second) != 2 || second[0].name != "cristales" || second[1].name != "roto" {
		t.Fatalf("proyectos después = %+v", second)
	}
	if second[0].id != first[0].id {
		t.Error("el proyecto cambió de ID al actualizarse")
	}
	if second[0].healthState != "" || second[0].healthCheckedAtOK {
		t.Errorf("el resultado del healthcheck no se borró: %+v", second[0])
	}
	if got := invalidFiles(t, pool, server); len(got) != 0 {
		t.Errorf("quedaron inválidos: %v", got)
	}
}

// Una lectura más vieja que la guardada (un reenvío que llegó tarde) no se
// aplica, y con error se conserva lo anterior.
func TestIngestKeepsTheLastGoodRead(t *testing.T) {
	pool, server, i := setup(t)
	mustIngest(t, i, server, read(t0, docker("cristales")))

	if mustIngest(t, i, server, read(t0.Add(-time.Minute))) {
		t.Error("se aplicó una lectura más vieja")
	}
	failed := proto.ProjectsState{TakenAt: t0.Add(time.Minute), Source: proto.RuntimeSource{Status: proto.SourceError, Detail: "permiso denegado"}}
	if !mustIngest(t, i, server, failed) {
		t.Error("la lectura con error no avanzó la marca")
	}
	if got := stored(t, pool, server); len(got) != 1 {
		t.Errorf("con error se perdieron proyectos: %+v", got)
	}
	var status, detail string
	if err := pool.QueryRow(t.Context(), "SELECT projects_status, projects_detail FROM servers WHERE id = $1", server).
		Scan(&status, &detail); err != nil || status != "error" || detail != "permiso denegado" {
		t.Errorf("estado de la lectura = %q %q (%v)", status, detail, err)
	}

	// Sin directorio, el servidor no tiene proyectos.
	gone := proto.ProjectsState{TakenAt: t0.Add(2 * time.Minute), Source: proto.RuntimeSource{Status: proto.SourceUnavailable}}
	mustIngest(t, i, server, gone)
	if got := stored(t, pool, server); len(got) != 0 {
		t.Errorf("sin directorio quedaron proyectos: %+v", got)
	}
}

func TestIngestRejectsReadsFromTheFuture(t *testing.T) {
	_, server, i := setup(t)
	if _, err := i.Ingest(t.Context(), server, read(t0.Add(time.Hour))); !errors.Is(err, ErrInvalidState) {
		t.Errorf("Ingest = %v, se esperaba ErrInvalidState", err)
	}
}

// Los proyectos se van con el servidor.
func TestProjectsAreDeletedWithTheirServer(t *testing.T) {
	pool, server, i := setup(t)
	state := read(t0, docker("cristales"))
	state.Invalid = []proto.InvalidProject{{File: "x.yaml", Error: "name: falta"}}
	mustIngest(t, i, server, state)

	if _, err := store.New(pool).DeleteServer(t.Context(), server); err != nil {
		t.Fatalf("borrar el servidor: %v", err)
	}
	var n int
	if err := pool.QueryRow(t.Context(),
		"SELECT (SELECT count(*) FROM projects) + (SELECT count(*) FROM project_invalid_files)").Scan(&n); err != nil || n != 0 {
		t.Errorf("quedaron %d filas (%v)", n, err)
	}
}
