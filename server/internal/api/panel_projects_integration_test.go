//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

type projectBody struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	ServerName string    `json:"server_name"`
	Status     string    `json:"status"`
	Units      struct {
		Total   int `json:"total"`
		Healthy int `json:"healthy"`
	} `json:"units"`
	Health *struct {
		URL   string `json:"url"`
		State string `json:"state"`
	} `json:"health"`
	MissingUnits []string          `json:"missing_units"`
	Containers   []json.RawMessage `json:"containers"`
	Services     []json.RawMessage `json:"services"`
}

type projectListBody struct {
	Projects []projectBody `json:"projects"`
	Invalid  []struct {
		ServerName string `json:"server_name"`
		File       string `json:"file"`
		Error      string `json:"error"`
	} `json:"invalid"`
	ReadErrors []json.RawMessage `json:"read_errors"`
}

// sendProjects manda una lectura de proyectos y espera su acuse.
func sendProjects(t *testing.T, conn *websocket.Conn, state proto.ProjectsState) {
	t.Helper()
	sendEnvelope(t, conn, state)
	if envelope := receive(t, conn); envelope.Type != proto.TypeProjectsStateAck {
		t.Fatalf("llegó %q, se esperaba projects_state_ack", envelope.Type)
	}
}

// declaredProjects son tres proyectos sobre el runtime de runtimeStateFor:
//   - web: Docker, su contenedor corre y el healthcheck pasa → ok.
//   - api: Docker, su contenedor se reinicia → down.
//   - facturas: systemd, uno de sus dos servicios no existe → degraded.
func declaredProjects(at time.Time) proto.ProjectsState {
	health := &proto.HealthcheckSpec{URL: "https://web.example.com/health", ExpectStatus: 200, TimeoutSeconds: 10}
	return proto.ProjectsState{
		TakenAt: at,
		Source:  proto.RuntimeSource{Status: proto.SourceOK},
		Projects: []proto.Project{
			{
				Name: "api", Environment: proto.EnvProduction, Repo: "github.com/x/api", Branch: "main",
				Runtime: proto.RuntimeDocker, Workdir: "/srv/api", ComposeFile: "compose.yml", ComposeProject: "api",
			},
			{
				Name: "facturas", Environment: proto.EnvStaging, Repo: "github.com/x/facturas", Branch: "main",
				Runtime: proto.RuntimeSystemd, Workdir: "/srv/facturas",
				Units: []string{"nginx.service", "gunicorn@facturas.service"},
			},
			{
				Name: "web", Environment: proto.EnvProduction, Repo: "github.com/x/web", Branch: "main",
				Runtime: proto.RuntimeDocker, Workdir: "/srv/web", ComposeFile: "compose.yml", ComposeProject: "web",
				Healthcheck: health,
				Health:      &proto.HealthResult{State: proto.HealthPassing, CheckedAt: at, StatusCode: 200, LatencyMS: 40},
			},
		},
		Invalid: []proto.InvalidProject{{File: "roto.yaml", Error: "name: falta"}},
	}
}

func runtimeStateFor(at time.Time) proto.RuntimeState {
	state := runtimeState(at)
	state.Containers = []proto.Container{
		{ID: strings.Repeat("a1", 32), Name: "web-1", Image: "web:1", State: proto.ContainerRunning, ComposeProject: "web"},
		// Una migración que terminó no cuenta para el estado.
		{ID: strings.Repeat("a2", 32), Name: "web-migrate", Image: "web:1", State: proto.ContainerExited, ComposeProject: "web"},
		{ID: strings.Repeat("b1", 32), Name: "api-1", Image: "api:1", State: proto.ContainerRestarting, ComposeProject: "api"},
	}
	return state
}

func (f *fixture) getProjects(t *testing.T, cookie *http.Cookie) projectListBody {
	t.Helper()
	resp := f.do(t, http.MethodGet, "/v1/projects", "", "198.51.100.80", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/projects = %d: %s", resp.StatusCode, resp.body)
	}
	return decodeJSON[projectListBody](t, resp)
}

func TestProjectsAPI(t *testing.T) {
	f := setup(t)
	cookie := f.login(t, "198.51.100.80")
	agent, _ := connectAgent(t, f, "vps-proyectos-api", "203.0.113.80")

	at := time.Now().UTC().Truncate(time.Millisecond)
	sendRuntime(t, agent, runtimeStateFor(at))
	sendProjects(t, agent, declaredProjects(at))

	list := f.getProjects(t, cookie)
	if len(list.Projects) != 3 {
		t.Fatalf("proyectos = %+v", list.Projects)
	}
	want := map[string]struct {
		status         string
		total, healthy int
	}{
		"api":      {"down", 1, 0},
		"facturas": {"degraded", 2, 1},
		"web":      {"ok", 1, 1},
	}
	for _, p := range list.Projects {
		w := want[p.Name]
		if p.Status != w.status || p.Units.Total != w.total || p.Units.Healthy != w.healthy {
			t.Errorf("%s = %s con %d/%d; se esperaba %s con %d/%d",
				p.Name, p.Status, p.Units.Healthy, p.Units.Total, w.status, w.healthy, w.total)
		}
		if p.ServerName != "vps-proyectos-api" {
			t.Errorf("%s: servidor = %q", p.Name, p.ServerName)
		}
	}
	if len(list.Invalid) != 1 || list.Invalid[0].File != "roto.yaml" || list.Invalid[0].ServerName != "vps-proyectos-api" {
		t.Errorf("inválidos = %+v", list.Invalid)
	}

	// El detalle trae lo que corre y lo que falta.
	byName := map[string]projectBody{}
	for _, p := range list.Projects {
		byName[p.Name] = p
	}
	resp := f.do(t, http.MethodGet, "/v1/projects/"+byName["facturas"].ID.String(), "", "198.51.100.80", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detalle = %d: %s", resp.StatusCode, resp.body)
	}
	facturas := decodeJSON[projectBody](t, resp)
	if len(facturas.Services) != 1 || len(facturas.MissingUnits) != 1 || facturas.MissingUnits[0] != "gunicorn@facturas.service" {
		t.Errorf("detalle de facturas = %+v", facturas)
	}
	web := decodeJSON[projectBody](t, f.do(t, http.MethodGet, "/v1/projects/"+byName["web"].ID.String(), "", "198.51.100.80", cookie, nil))
	if len(web.Containers) != 2 || web.Health == nil || web.Health.State != "passing" {
		t.Errorf("detalle de web = %+v", web)
	}

	// Si el healthcheck empieza a fallar, el proyecto queda degradado.
	failing := declaredProjects(at.Add(time.Second))
	failing.Projects[2].Health = &proto.HealthResult{State: proto.HealthFailing, CheckedAt: at, LatencyMS: 10000, Detail: "timeout"}
	sendProjects(t, agent, failing)
	for _, p := range f.getProjects(t, cookie).Projects {
		if p.Name == "web" && p.Status != "degraded" {
			t.Errorf("web con el healthcheck fallando = %s, se esperaba degraded", p.Status)
		}
	}
}

func TestProjectsAPIRejects(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.81"
	for path, want := range map[string]int{
		"/v1/projects":                      http.StatusUnauthorized,
		"/v1/projects/" + uuid.Nil.String(): http.StatusUnauthorized,
	} {
		if resp := f.do(t, http.MethodGet, path, "", ip, nil, nil); resp.StatusCode != want {
			t.Errorf("GET %s sin sesión = %d, se esperaba %d", path, resp.StatusCode, want)
		}
	}
	cookie := f.login(t, ip)
	for path, want := range map[string]int{
		"/v1/projects/no-es-uuid":                          http.StatusBadRequest,
		"/v1/projects/" + uuid.Must(uuid.NewV7()).String(): http.StatusNotFound,
	} {
		if resp := f.do(t, http.MethodGet, path, "", ip, cookie, nil); resp.StatusCode != want {
			t.Errorf("GET %s = %d, se esperaba %d", path, resp.StatusCode, want)
		}
	}
	// Sin proyectos, listas vacías y no null.
	list := f.getProjects(t, cookie)
	if list.Projects == nil || list.Invalid == nil || list.ReadErrors == nil {
		t.Error("la lista vacía trae null")
	}
}

// Un panel que mira el servidor se entera cuando cambian sus proyectos.
func TestPanelIsToldWhenProjectsChange(t *testing.T) {
	f := setup(t)
	cookie := f.login(t, "198.51.100.82")
	agent, serverID := connectAgent(t, f, "vps-proyectos-vivo", "203.0.113.82")

	panelConn, status, err := f.dialPanel(t, cookie)
	if err != nil {
		t.Fatalf("abrir el canal del panel: %v (estado %d)", err, status)
	}
	readPanel(t, panelConn) // bienvenida
	writePanel(t, panelConn, `{"type":"subscribe","server_ids":["`+serverID.String()+`"]}`)
	if ack := readPanel(t, panelConn); ack.Type != "subscribed" {
		t.Fatalf("confirmación = %+v", ack)
	}

	sendProjects(t, agent, declaredProjects(time.Now().UTC().Truncate(time.Millisecond)))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, raw, err := panelConn.Read(ctx)
	if err != nil {
		t.Fatalf("leer del canal del panel: %v", err)
	}
	var message struct {
		Type     string    `json:"type"`
		ServerID uuid.UUID `json:"server_id"`
	}
	if err := json.Unmarshal(raw, &message); err != nil || message.Type != "projects" || message.ServerID != serverID {
		t.Errorf("mensaje = %s (%v)", raw, err)
	}
}
