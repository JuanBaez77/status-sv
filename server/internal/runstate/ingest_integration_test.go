//go:build integration

package runstate

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/store"
	"github.com/JuanBaez77/status-sv/server/internal/testdb"
)

var t0 = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func setup(t *testing.T) (*pgxpool.Pool, *store.Queries, uuid.UUID, *Ingester) {
	t.Helper()
	pool := testdb.Setup(t)
	testdb.Truncate(t, pool)
	q := store.New(pool)

	id := uuid.Must(uuid.NewV7())
	if _, err := q.CreateServer(t.Context(), store.CreateServerParams{
		ID: id, Name: "vps-" + id.String()[:8], Hostname: "vps", Labels: []byte(`{}`), AgentVersion: "0.1.0",
	}); err != nil {
		t.Fatalf("crear el servidor: %v", err)
	}
	i := NewIngester(pool)
	i.now = func() time.Time { return t0 }
	return pool, q, id, i
}

func container(id, name string) proto.Container {
	return proto.Container{
		ID: strings.Repeat(id, 64/len(id)), Name: name, Image: "nginx:1.27", State: proto.ContainerRunning,
		StartedAt: t0.Add(-time.Hour), Usage: &proto.Usage{CPUPercent: 1.5, MemoryBytes: 50 << 20},
	}
}

func fullState(at time.Time) proto.RuntimeState {
	ok := proto.RuntimeSource{Status: proto.SourceOK}
	return proto.RuntimeState{
		TakenAt:    at,
		Sources:    proto.RuntimeSources{Docker: ok, Systemd: ok, Processes: ok},
		Containers: []proto.Container{container("a", "web"), container("b", "db")},
		Services: []proto.Service{{
			Unit: "nginx.service", LoadState: "loaded", ActiveState: proto.ActiveActive, SubState: "running",
			MainPID: 812, ActiveSince: t0.Add(-24 * time.Hour),
		}},
		Processes: []proto.Process{{
			PID: 900, Name: "python3", Command: "python3 importar.py --db-password=***", User: "juan",
			Usage: proto.Usage{CPUPercent: 25, MemoryBytes: 100 << 20},
		}},
		Omitted: proto.RuntimeOmitted{Processes: 3},
	}
}

func units(t *testing.T, q *store.Queries, server uuid.UUID) map[string]store.ListRuntimeUnitsRow {
	t.Helper()
	rows, err := q.ListRuntimeUnits(t.Context(), server)
	if err != nil {
		t.Fatalf("listar unidades: %v", err)
	}
	out := make(map[string]store.ListRuntimeUnitsRow, len(rows))
	for _, r := range rows {
		out[r.Kind+"/"+r.Name] = r
	}
	return out
}

func sources(t *testing.T, q *store.Queries, server uuid.UUID) map[string]store.ListRuntimeSourcesRow {
	t.Helper()
	rows, err := q.ListRuntimeSources(t.Context(), server)
	if err != nil {
		t.Fatalf("listar fuentes: %v", err)
	}
	out := make(map[string]store.ListRuntimeSourcesRow, len(rows))
	for _, r := range rows {
		out[r.Source] = r
	}
	return out
}

func TestIngestStoresEverything(t *testing.T) {
	_, q, server, ingester := setup(t)

	applied, err := ingester.Ingest(t.Context(), server, fullState(t0))
	if err != nil || !applied {
		t.Fatalf("Ingest: %v (aplicada %v)", err, applied)
	}

	got := units(t, q, server)
	if len(got) != 4 {
		t.Fatalf("esperaba 4 unidades, hay %d: %v", len(got), got)
	}
	web := got["container/web"]
	if web.State != "running" || web.Image != "nginx:1.27" || web.StartedAt == nil ||
		!web.CpuPercent.Valid || web.CpuPercent.Float32 != 1.5 || web.MemoryBytes.Int64 != 50<<20 {
		t.Errorf("contenedor mal guardado: %+v", web)
	}
	nginx := got["service/nginx.service"]
	if nginx.Pid != 812 || nginx.SubState != "running" || nginx.CpuPercent.Valid {
		t.Errorf("servicio mal guardado (sin consumo tiene que quedar NULL): %+v", nginx)
	}
	py := got["process/python3"]
	if py.UnitKey != "900" || py.UserName != "juan" || !strings.Contains(py.Command, "***") || py.StartedAt != nil {
		t.Errorf("proceso mal guardado (sin arranque tiene que quedar NULL): %+v", py)
	}

	src := sources(t, q, server)
	if len(src) != 3 || src["processes"].Omitted != 3 || src["docker"].Status != "ok" {
		t.Errorf("fuentes mal guardadas: %+v", src)
	}
}

// Cómo terminó un servicio parado se guarda: es lo que le dice al panel si es
// un problema.
func TestIngestStoresServiceResult(t *testing.T) {
	_, q, server, ingester := setup(t)

	state := fullState(t0)
	state.Services = append(state.Services, proto.Service{
		Unit: "cloud-init-main.service", LoadState: "loaded", ActiveState: proto.ActiveInactive,
		SubState: "dead", Result: "success",
	})
	if _, err := ingester.Ingest(t.Context(), server, state); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	got := units(t, q, server)
	if r := got["service/cloud-init-main.service"].Result; r != "success" {
		t.Errorf("result del servicio parado: %q", r)
	}
	if r := got["service/nginx.service"].Result; r != "" {
		t.Errorf("un servicio corriendo no trae result: %q", r)
	}
}

func TestIngestReplacesWhatChanged(t *testing.T) {
	_, q, server, ingester := setup(t)
	if _, err := ingester.Ingest(t.Context(), server, fullState(t0)); err != nil {
		t.Fatalf("primera foto: %v", err)
	}

	// En la segunda, "db" ya no está y "web" se detuvo.
	next := fullState(t0.Add(30 * time.Second))
	stopped := container("a", "web")
	stopped.State, stopped.Usage = proto.ContainerExited, nil
	next.Containers = []proto.Container{stopped}
	if _, err := ingester.Ingest(t.Context(), server, next); err != nil {
		t.Fatalf("segunda foto: %v", err)
	}

	got := units(t, q, server)
	if _, ok := got["container/db"]; ok {
		t.Error("el contenedor que ya no está tendría que borrarse")
	}
	web := got["container/web"]
	if web.State != "exited" || web.CpuPercent.Valid || !web.SeenAt.Equal(t0.Add(30*time.Second)) {
		t.Errorf("el contenedor detenido no se actualizó: %+v", web)
	}
}

// Una fuente que falló conserva lo último que se supo; una que no existe borra
// lo que hubiera.
func TestIngestRespectsSourceStatus(t *testing.T) {
	_, q, server, ingester := setup(t)
	if _, err := ingester.Ingest(t.Context(), server, fullState(t0)); err != nil {
		t.Fatalf("primera foto: %v", err)
	}

	next := fullState(t0.Add(30 * time.Second))
	next.Sources.Docker = proto.RuntimeSource{Status: proto.SourceError, Detail: "Docker no contestó"}
	next.Containers = nil
	next.Sources.Systemd = proto.RuntimeSource{Status: proto.SourceUnavailable, Detail: "sin systemd"}
	next.Services = nil
	if _, err := ingester.Ingest(t.Context(), server, next); err != nil {
		t.Fatalf("segunda foto: %v", err)
	}

	got := units(t, q, server)
	if _, ok := got["container/web"]; !ok {
		t.Error("con Docker en error, los contenedores se conservan")
	}
	if _, ok := got["service/nginx.service"]; ok {
		t.Error("con systemd no disponible, los servicios se borran")
	}
	if src := sources(t, q, server)["docker"]; src.Status != "error" || src.Detail != "Docker no contestó" {
		t.Errorf("la fuente en error no quedó registrada: %+v", src)
	}
}

// Una foto más vieja que la guardada (un reenvío que llegó tarde) no pisa a la
// nueva, pero se confirma igual.
func TestIngestIgnoresOlderSnapshots(t *testing.T) {
	_, q, server, ingester := setup(t)
	if _, err := ingester.Ingest(t.Context(), server, fullState(t0)); err != nil {
		t.Fatalf("foto nueva: %v", err)
	}

	old := fullState(t0.Add(-time.Minute))
	old.Containers = nil
	applied, err := ingester.Ingest(t.Context(), server, old)
	if err != nil || applied {
		t.Fatalf("la foto vieja no se aplica y no es error: aplicada %v, error %v", applied, err)
	}
	if len(units(t, q, server)) != 4 {
		t.Error("la foto vieja borró unidades")
	}

	// Y la misma foto otra vez, igual.
	if applied, err := ingester.Ingest(t.Context(), server, fullState(t0)); err != nil || applied {
		t.Errorf("la misma foto repetida: aplicada %v, error %v", applied, err)
	}
}

func TestIngestRejectsFutureSnapshots(t *testing.T) {
	_, _, server, ingester := setup(t)
	_, err := ingester.Ingest(t.Context(), server, fullState(t0.Add(MaxClockSkew+time.Minute)))
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("esperaba ErrInvalidState, obtuve %v", err)
	}
}

func TestIngestEmptyServerSources(t *testing.T) {
	_, q, server, ingester := setup(t)
	unavailable := proto.RuntimeSource{Status: proto.SourceUnavailable}
	state := proto.RuntimeState{
		TakenAt: t0,
		Sources: proto.RuntimeSources{Docker: unavailable, Systemd: unavailable, Processes: unavailable},
	}
	if _, err := ingester.Ingest(t.Context(), server, state); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if n := len(units(t, q, server)); n != 0 {
		t.Errorf("no tendría que haber unidades, hay %d", n)
	}
}
