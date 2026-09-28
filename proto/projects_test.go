package proto_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/proto"
)

func dockerProject() proto.Project {
	return proto.Project{
		Name:           "cristales-baez",
		Environment:    proto.EnvProduction,
		Repo:           "github.com/extendsystem/cristales-baez",
		Branch:         "main",
		Runtime:        proto.RuntimeDocker,
		Workdir:        "/srv/cristales-baez",
		ComposeFile:    "docker-compose.yml",
		ComposeProject: "cristales-baez",
		Healthcheck: &proto.HealthcheckSpec{
			URL: "https://cristales.example.com/health", ExpectStatus: 200, TimeoutSeconds: 10,
		},
		Health: &proto.HealthResult{
			State:      proto.HealthPassing,
			CheckedAt:  time.Date(2026, 9, 20, 11, 59, 30, 0, time.UTC),
			StatusCode: 200,
			LatencyMS:  84,
		},
	}
}

func systemdProject() proto.Project {
	return proto.Project{
		Name:        "facturas",
		Environment: proto.EnvStaging,
		Repo:        "git@github.com:extendsystem/facturas.git",
		Branch:      "release/2026.09",
		Runtime:     proto.RuntimeSystemd,
		Workdir:     "/srv/facturas",
		Units:       []string{"gunicorn@facturas.service"},
	}
}

func validProjectsState() proto.ProjectsState {
	return proto.ProjectsState{
		TakenAt:  time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
		Source:   proto.RuntimeSource{Status: proto.SourceOK},
		Projects: []proto.Project{dockerProject(), systemdProject()},
		Invalid: []proto.InvalidProject{
			{File: "cotizador.yaml", Error: "healthcheck.url: la URL tiene que ser http o https"},
		},
	}
}

// TestProjectsStateValidate recorre la frontera de los proyectos declarados:
// qué se acepta desde la red y qué se rechaza antes de llegar a la base, al
// panel o, más adelante, a los parámetros de un deploy.
func TestProjectsStateValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*proto.ProjectsState)
		wantErr bool
	}{
		{name: "válido", mutate: func(*proto.ProjectsState) {}},
		{
			name: "sin directorio de proyectos",
			mutate: func(p *proto.ProjectsState) {
				p.Source = proto.RuntimeSource{Status: proto.SourceUnavailable, Detail: "no existe /etc/status-sv/projects.d"}
				p.Projects, p.Invalid = nil, nil
			},
		},
		{
			name: "con error y proyectos",
			mutate: func(p *proto.ProjectsState) {
				p.Source = proto.RuntimeSource{Status: proto.SourceError, Detail: "permiso denegado"}
			},
			wantErr: true,
		},
		{name: "sin marca de tiempo", mutate: func(p *proto.ProjectsState) { p.TakenAt = time.Time{} }, wantErr: true},
		{
			name:    "nombre repetido",
			mutate:  func(p *proto.ProjectsState) { p.Projects[1].Name = p.Projects[0].Name },
			wantErr: true,
		},
		{
			name: "demasiados proyectos",
			mutate: func(p *proto.ProjectsState) {
				p.Invalid = nil
				p.Projects = nil
				for i := range proto.MaxProjects + 1 {
					project := systemdProject()
					project.Name = fmt.Sprintf("proyecto-%d", i)
					p.Projects = append(p.Projects, project)
				}
			},
			wantErr: true,
		},
		{
			name:    "archivo inválido con barra",
			mutate:  func(p *proto.ProjectsState) { p.Invalid[0].File = "../etc/passwd" },
			wantErr: true,
		},
		{
			name:    "archivo inválido sin explicación",
			mutate:  func(p *proto.ProjectsState) { p.Invalid[0].Error = "" },
			wantErr: true,
		},
		{
			name:    "explicación con un salto de línea",
			mutate:  func(p *proto.ProjectsState) { p.Invalid[0].Error = "línea 1\nDATABASE_PASSWORD=x" },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := validProjectsState()
			tt.mutate(&state)
			if err := state.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, se esperaba error: %v", err, tt.wantErr)
			}
		})
	}
}

func TestProjectValidate(t *testing.T) {
	tests := []struct {
		name    string
		base    func() proto.Project
		mutate  func(*proto.Project)
		wantErr bool
	}{
		{name: "docker válido", base: dockerProject, mutate: func(*proto.Project) {}},
		{name: "systemd válido", base: systemdProject, mutate: func(*proto.Project) {}},
		{name: "sin healthcheck", base: systemdProject, mutate: func(p *proto.Project) { p.Healthcheck, p.Health = nil, nil }},
		{name: "nombre con mayúsculas", base: dockerProject, mutate: func(p *proto.Project) { p.Name = "Cristales" }, wantErr: true},
		{name: "nombre con punto", base: dockerProject, mutate: func(p *proto.Project) { p.Name = "a.b" }, wantErr: true},
		{name: "nombre largo", base: dockerProject, mutate: func(p *proto.Project) { p.Name = strings.Repeat("a", 64) }, wantErr: true},
		{name: "entorno inventado", base: dockerProject, mutate: func(p *proto.Project) { p.Environment = "prod" }, wantErr: true},
		{name: "runtime inventado", base: dockerProject, mutate: func(p *proto.Project) { p.Runtime = "podman" }, wantErr: true},
		{name: "repo con espacio", base: dockerProject, mutate: func(p *proto.Project) { p.Repo = "github.com/a b" }, wantErr: true},
		{name: "repo con punto y coma", base: dockerProject, mutate: func(p *proto.Project) { p.Repo = "x;rm -rf /" }, wantErr: true},
		{name: "rama con ..", base: dockerProject, mutate: func(p *proto.Project) { p.Branch = "main..x" }, wantErr: true},
		{name: "rama que empieza con guion", base: dockerProject, mutate: func(p *proto.Project) { p.Branch = "-x" }, wantErr: true},
		{name: "rama con espacio", base: dockerProject, mutate: func(p *proto.Project) { p.Branch = "mi rama" }, wantErr: true},
		{name: "workdir relativo", base: dockerProject, mutate: func(p *proto.Project) { p.Workdir = "srv/x" }, wantErr: true},
		{name: "workdir raíz", base: dockerProject, mutate: func(p *proto.Project) { p.Workdir = "/" }, wantErr: true},
		{name: "workdir con ..", base: dockerProject, mutate: func(p *proto.Project) { p.Workdir = "/srv/../etc" }, wantErr: true},
		{name: "workdir con barra final", base: dockerProject, mutate: func(p *proto.Project) { p.Workdir = "/srv/x/" }, wantErr: true},
		{name: "compose_file que sale de workdir", base: dockerProject, mutate: func(p *proto.Project) { p.ComposeFile = "../otro/compose.yml" }, wantErr: true},
		{name: "compose_file absoluto", base: dockerProject, mutate: func(p *proto.Project) { p.ComposeFile = "/etc/compose.yml" }, wantErr: true},
		{name: "compose_file en subdirectorio", base: dockerProject, mutate: func(p *proto.Project) { p.ComposeFile = "deploy/compose.prod.yml" }},
		{name: "docker sin compose_project", base: dockerProject, mutate: func(p *proto.Project) { p.ComposeProject = "" }, wantErr: true},
		{name: "docker con units", base: dockerProject, mutate: func(p *proto.Project) { p.Units = []string{"x.service"} }, wantErr: true},
		{name: "systemd sin units", base: systemdProject, mutate: func(p *proto.Project) { p.Units = nil }, wantErr: true},
		{name: "systemd con compose", base: systemdProject, mutate: func(p *proto.Project) { p.ComposeFile = "compose.yml" }, wantErr: true},
		{name: "unit que no es servicio", base: systemdProject, mutate: func(p *proto.Project) { p.Units = []string{"x.timer"} }, wantErr: true},
		{name: "unit repetida", base: systemdProject, mutate: func(p *proto.Project) { p.Units = []string{"a.service", "a.service"} }, wantErr: true},
		{
			name: "demasiadas units",
			base: systemdProject,
			mutate: func(p *proto.Project) {
				p.Units = nil
				for i := range proto.MaxProjectUnits + 1 {
					p.Units = append(p.Units, fmt.Sprintf("u%d.service", i))
				}
			},
			wantErr: true,
		},
		{name: "healthcheck ftp", base: dockerProject, mutate: func(p *proto.Project) { p.Healthcheck.URL = "ftp://x/health" }, wantErr: true},
		{name: "healthcheck con usuario", base: dockerProject, mutate: func(p *proto.Project) { p.Healthcheck.URL = "https://juan:clave@x/health" }, wantErr: true},
		{name: "healthcheck sin servidor", base: dockerProject, mutate: func(p *proto.Project) { p.Healthcheck.URL = "https:///health" }, wantErr: true},
		{name: "healthcheck con puerto y query", base: dockerProject, mutate: func(p *proto.Project) { p.Healthcheck.URL = "http://127.0.0.1:8000/health?full=1" }},
		{name: "expect_status imposible", base: dockerProject, mutate: func(p *proto.Project) { p.Healthcheck.ExpectStatus = 700 }, wantErr: true},
		{name: "timeout de más", base: dockerProject, mutate: func(p *proto.Project) { p.Healthcheck.TimeoutSeconds = 31 }, wantErr: true},
		{name: "timeout cero", base: dockerProject, mutate: func(p *proto.Project) { p.Healthcheck.TimeoutSeconds = 0 }, wantErr: true},
		{name: "resultado sin healthcheck", base: dockerProject, mutate: func(p *proto.Project) { p.Healthcheck = nil }, wantErr: true},
		{name: "resultado con estado inventado", base: dockerProject, mutate: func(p *proto.Project) { p.Health.State = "ok" }, wantErr: true},
		{name: "resultado sin fecha", base: dockerProject, mutate: func(p *proto.Project) { p.Health.CheckedAt = time.Time{} }, wantErr: true},
		{
			name: "resultado fallido sin respuesta",
			base: dockerProject,
			mutate: func(p *proto.Project) {
				p.Health = &proto.HealthResult{State: proto.HealthFailing, CheckedAt: p.Health.CheckedAt, LatencyMS: 10000, Detail: "timeout"}
			},
		},
		{name: "detalle con control", base: dockerProject, mutate: func(p *proto.Project) { p.Health.Detail = "a\x1b[31mb" }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project := tt.base()
			tt.mutate(&project)
			if err := project.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, se esperaba error: %v", err, tt.wantErr)
			}
		})
	}
}

func TestProjectsStateAckValidate(t *testing.T) {
	if err := (proto.ProjectsStateAck{}).Validate(); err == nil {
		t.Error("un acuse sin marca de tiempo pasó")
	}
	if err := (proto.ProjectsStateAck{TakenAt: time.Now()}).Validate(); err != nil {
		t.Errorf("un acuse válido no pasó: %v", err)
	}
}

// Los errores de un proyecto viajan al control plane como explicación de un
// archivo inválido: nombran el campo, nunca el valor (ADR-012).
func TestProjectErrorsNeverIncludeTheValue(t *testing.T) {
	const secret = "s3cr3t0"
	mutations := map[string]func(*proto.Project){
		"name":         func(p *proto.Project) { p.Name = "X" + secret },
		"environment":  func(p *proto.Project) { p.Environment = secret },
		"repo":         func(p *proto.Project) { p.Repo = "github.com/x " + secret },
		"branch":       func(p *proto.Project) { p.Branch = secret + ".." },
		"workdir":      func(p *proto.Project) { p.Workdir = "srv/" + secret },
		"compose_file": func(p *proto.Project) { p.ComposeFile = "../" + secret },
		"runtime":      func(p *proto.Project) { p.Runtime = secret },
		"url": func(p *proto.Project) {
			p.Healthcheck.URL = "https://juan:" + secret + "@example.com/health"
		},
		"url ftp": func(p *proto.Project) { p.Healthcheck.URL = "ftp://" + secret + "/x" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			project := dockerProject()
			mutate(&project)
			err := project.Validate()
			if err == nil {
				t.Fatal("se aceptó un valor inválido")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("el error muestra el valor: %v", err)
			}
		})
	}
}
