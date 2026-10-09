package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/server/internal/tokens"
)

var projTwin = uuid.MustParse("0192f5e0-0000-7000-8000-0000000000b2")

// withProjectDetails completa los proyectos del fixture con su estado, su
// detalle y su historia.
func withProjectDetails(f *fixture) {
	for i := range f.backend.projects {
		p := &f.backend.projects[i]
		p.Runtime, p.Repo, p.Branch, p.Status = "docker", "github.com/a/"+p.Name, "main", "ok"
		p.UnitsTotal, p.UnitsHealthy, p.SeenAt = 2, 2, time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	}
	cpu, memory := 3.14159, int64(64<<20)
	started := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	shop := f.backend.projects[0]
	shop.Health = &HealthInfo{URL: "https://tienda.example/health", State: "ok", StatusCode: 200, LatencyMS: 54}
	f.backend.details = map[uuid.UUID]ProjectDetail{
		projShop: {
			ProjectInfo: shop, Workdir: "/srv/tienda", ComposeFile: "compose.yml", ComposeProject: "tienda",
			Containers: []ContainerInfo{
				{Name: "tienda-web-1", Service: "web", Image: "tienda-web", State: "running", Health: "healthy", StartedAt: &started, CPUPercent: &cpu, MemoryBytes: &memory},
			},
			SourceStatus: "ok", SiteDomain: "tienda.example", SitePort: 8001,
			EnvRequired: 2, EnvMissing: []string{"SECRET_KEY"},
		},
	}
	finished := func(d time.Duration) *time.Time { t := started.Add(d); return &t }
	f.backend.deploys = map[string][]DeployInfo{
		"tienda": {
			{ID: uuid.Must(uuid.NewV7()), Kind: "deploy", Status: "failed", Code: "build_failed", Message: "falló la compilación",
				PreviousCommit: "bbbb", RequestedBy: "juan", RequestedAt: started, StartedAt: finished(time.Minute), FinishedAt: finished(3 * time.Minute)},
			{ID: uuid.Must(uuid.NewV7()), Kind: "deploy", Status: "rolled_back", DeployedCommit: "cccc", PreviousCommit: "bbbb",
				RequestedBy: "juan", RequestedAt: started, StartedAt: finished(0), FinishedAt: finished(2 * time.Minute)},
			{ID: uuid.Must(uuid.NewV7()), Kind: "deploy", Status: "succeeded", DeployedCommit: "bbbb", PreviousCommit: "aaaa",
				RequestedBy: "juan", RequestedAt: started},
		},
	}
}

func TestListProjects(t *testing.T) {
	f := newFixture(t)
	withProjectDetails(f)
	f.addToken("staging", []tokens.Scope{"read:staging"})
	f.addToken("prod", []tokens.Scope{"read:production"})

	out, errText := call[listProjectsOutput](t, f.connect(t, "staging"), "list_projects", map[string]any{})
	if errText != "" || len(out.Projects) != 1 || out.Projects[0].Ref != "sv-gcg/demo" || out.Projects[0].Status != "ok" {
		t.Fatalf("staging ve solo demo: %+v %q", out, errText)
	}

	prod := f.connect(t, "prod")
	out, _ = call[listProjectsOutput](t, prod, "list_projects", map[string]any{})
	if len(out.Projects) != 2 {
		t.Errorf("producción ve tienda y cotizador: %+v", out.Projects)
	}
	out, _ = call[listProjectsOutput](t, prod, "list_projects", map[string]any{"server": "cristales"})
	if len(out.Projects) != 1 || out.Projects[0].Name != "cotizador" {
		t.Errorf("filtrado por servidor: %+v", out.Projects)
	}
	out, _ = call[listProjectsOutput](t, prod, "list_projects", map[string]any{"environment": "staging"})
	if len(out.Projects) != 0 {
		t.Errorf("filtrar por un entorno que no alcanza no da nada: %+v", out.Projects)
	}
	if _, errText := call[listProjectsOutput](t, prod, "list_projects", map[string]any{"environment": "dev"}); !strings.Contains(errText, "staging o production") {
		t.Errorf("entorno inventado: %q", errText)
	}
}

func TestGetProjectStatus(t *testing.T) {
	f := newFixture(t)
	withProjectDetails(f)
	f.addToken("prod", []tokens.Scope{"read:production"})
	session := f.connect(t, "prod")

	for _, ref := range []string{"sv-gcg/tienda", "tienda", projShop.String()} {
		out, errText := call[projectStatusOutput](t, session, "get_project_status", map[string]any{"project": ref})
		if errText != "" {
			t.Fatalf("%s: %s", ref, errText)
		}
		if out.Project.Ref != "sv-gcg/tienda" || out.Workdir != "/srv/tienda" || out.RuntimeSource != "ok" {
			t.Errorf("%s: %+v", ref, out)
		}
	}

	out, _ := call[projectStatusOutput](t, session, "get_project_status", map[string]any{"project": "tienda"})
	if len(out.Containers) != 1 || out.Containers[0].Name != "tienda-web-1" || *out.Containers[0].CPUPercent != 3.1 {
		t.Errorf("contenedores = %+v", out.Containers)
	}
	if out.Project.Healthcheck == nil || out.Project.Healthcheck.StatusCode != 200 {
		t.Errorf("healthcheck = %+v", out.Project.Healthcheck)
	}
	if out.Site == nil || out.Site.Domain != "tienda.example" || strings.Join(out.MissingEnv, ",") != "SECRET_KEY" {
		t.Errorf("sitio %+v, variables %v", out.Site, out.MissingEnv)
	}
	// El último deploy falló sin tocar nada; el anterior volvió atrás a bbbb.
	if out.Version == nil || out.Version.Commit != "bbbb" {
		t.Errorf("versión = %+v, se esperaba bbbb", out.Version)
	}
	if out.LastDeploy == nil || out.LastDeploy.Status != "failed" || out.LastDeploy.Code != "build_failed" ||
		out.LastDeploy.DurationSeconds == nil || *out.LastDeploy.DurationSeconds != 120 {
		t.Errorf("último deploy = %+v", out.LastDeploy)
	}

	entries := f.recorder.all()
	last := entries[len(entries)-1]
	if last.Action != "mcp.get_project_status" || last.TargetType != "project" || last.TargetID != projShop.String() || last.Detail["proyecto"] != "sv-gcg/tienda" {
		t.Errorf("renglón = %+v", last)
	}
}

func TestGetProjectStatusAmbiguousAndHidden(t *testing.T) {
	f := newFixture(t)
	f.backend.projects = append(f.backend.projects,
		ProjectInfo{ID: projTwin, ServerID: serverB, ServerName: "cristales", Name: "tienda", Environment: tokens.EnvProduction})
	withProjectDetails(f)
	f.addToken("prod", []tokens.Scope{"read:production"})
	f.addToken("solo-tienda", []tokens.Scope{"read:production"}, projShop)

	_, errText := call[projectStatusOutput](t, f.connect(t, "prod"), "get_project_status", map[string]any{"project": "tienda"})
	if !strings.Contains(errText, "cristales/tienda o sv-gcg/tienda") {
		t.Errorf("un nombre repetido pide el servidor: %q", errText)
	}

	restricted := f.connect(t, "solo-tienda")
	// Con la lista, el nombre solo ya no es ambiguo: el otro no lo alcanza.
	if _, errText := call[projectStatusOutput](t, restricted, "get_project_status", map[string]any{"project": "tienda"}); errText != "" {
		t.Errorf("con lista: %q", errText)
	}
	_, hidden := call[projectStatusOutput](t, restricted, "get_project_status", map[string]any{"project": "cristales/cotizador"})
	_, missing := call[projectStatusOutput](t, restricted, "get_project_status", map[string]any{"project": "cristales/nada"})
	if strings.ReplaceAll(hidden, "cotizador", "X") != strings.ReplaceAll(missing, "nada", "X") || !strings.Contains(hidden, "no hay un proyecto") {
		t.Errorf("uno que no alcanza y uno que no existe dan el mismo error: %q y %q", hidden, missing)
	}

	f.addToken("staging", []tokens.Scope{"read:staging"})
	if _, errText := call[projectStatusOutput](t, f.connect(t, "staging"), "get_project_status", map[string]any{"project": "sv-gcg/tienda"}); !strings.Contains(errText, "no hay un proyecto") {
		t.Errorf("read:staging no ve un proyecto de producción: %q", errText)
	}
}

func TestRunningVersion(t *testing.T) {
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	tests := map[string]struct {
		deploys []DeployInfo
		want    string
	}{
		"sin historia":             {nil, ""},
		"el último bien":           {[]DeployInfo{{Status: "succeeded", DeployedCommit: "b"}, {Status: "succeeded", DeployedCommit: "a"}}, "b"},
		"en curso no cuenta":       {[]DeployInfo{{Status: "running"}, {Status: "succeeded", DeployedCommit: "a"}}, "a"},
		"volvió atrás al anterior": {[]DeployInfo{{Status: "rolled_back", DeployedCommit: "c", PreviousCommit: "b"}}, "b"},
		"falló sin tocar nada":     {[]DeployInfo{{Status: "failed", PreviousCommit: "b"}, {Status: "succeeded", DeployedCommit: "a"}}, "a"},
		"un rollback que anduvo":   {[]DeployInfo{{Kind: "rollback", Status: "succeeded", DeployedCommit: "a", FinishedAt: &at}}, "a"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := runningVersion(tt.deploys)
			switch {
			case tt.want == "" && got != nil:
				t.Errorf("versión = %+v, se esperaba ninguna", got)
			case tt.want != "" && (got == nil || got.Commit != tt.want):
				t.Errorf("versión = %+v, se esperaba %q", got, tt.want)
			}
		})
	}
}
