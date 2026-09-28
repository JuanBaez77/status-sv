package proto_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/proto"
)

func validContainer() proto.Container {
	return proto.Container{
		ID:             strings.Repeat("ab", 32),
		Name:           "cristales-web-1",
		Image:          "ghcr.io/extendsystem/cristales:2026.09.20",
		State:          proto.ContainerRunning,
		Health:         proto.HealthHealthy,
		StartedAt:      time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC),
		ComposeProject: "cristales",
		ComposeService: "web",
		Usage:          &proto.Usage{CPUPercent: 1.5, MemoryBytes: 180 << 20},
	}
}

func validService() proto.Service {
	return proto.Service{
		Unit:        "nginx.service",
		LoadState:   "loaded",
		ActiveState: proto.ActiveActive,
		SubState:    "running",
		MainPID:     812,
		ActiveSince: time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC),
		Usage:       &proto.Usage{CPUPercent: 0.1, MemoryBytes: 12 << 20},
	}
}

// stoppedService es un servicio habilitado que corrió y terminó bien: trae
// cómo terminó, pero no PID ni consumo.
func stoppedService() proto.Service {
	return proto.Service{
		Unit:        "cloud-init-main.service",
		LoadState:   "loaded",
		ActiveState: proto.ActiveInactive,
		SubState:    "dead",
		Result:      "success",
	}
}

func validProcess() proto.Process {
	return proto.Process{
		PID:       4242,
		Name:      "gunicorn",
		Command:   "/srv/app/.venv/bin/gunicorn app:wsgi --bind 127.0.0.1:8000",
		User:      "www-data",
		StartedAt: time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC),
		Usage:     proto.Usage{CPUPercent: 2.3, MemoryBytes: 95 << 20},
	}
}

func okSources() proto.RuntimeSources {
	ok := proto.RuntimeSource{Status: proto.SourceOK}
	return proto.RuntimeSources{Docker: ok, Systemd: ok, Processes: ok}
}

func validRuntimeState() proto.RuntimeState {
	return proto.RuntimeState{
		TakenAt:    time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
		Sources:    okSources(),
		Containers: []proto.Container{validContainer()},
		Services:   []proto.Service{validService(), stoppedService()},
		Processes:  []proto.Process{validProcess()},
	}
}

// TestRuntimeStateValidate recorre la frontera del estado de runtime: qué se
// acepta desde la red y qué se rechaza antes de llegar a la base o al panel.
func TestRuntimeStateValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*proto.RuntimeState)
		wantErr bool
	}{
		{name: "válido", mutate: func(*proto.RuntimeState) {}},
		{
			name: "vacío con las fuentes al día",
			mutate: func(r *proto.RuntimeState) {
				r.Containers, r.Services, r.Processes = nil, nil, nil
			},
		},
		{
			name: "servidor sin Docker",
			mutate: func(r *proto.RuntimeState) {
				r.Sources.Docker = proto.RuntimeSource{Status: proto.SourceUnavailable, Detail: "no existe /run/docker.sock"}
				r.Containers = nil
			},
		},
		{name: "sin marca de tiempo", mutate: func(r *proto.RuntimeState) { r.TakenAt = time.Time{} }, wantErr: true},
		{name: "fuente sin estado", mutate: func(r *proto.RuntimeState) { r.Sources.Systemd = proto.RuntimeSource{} }, wantErr: true},
		{
			name:    "fuente con estado inventado",
			mutate:  func(r *proto.RuntimeState) { r.Sources.Systemd.Status = "masomenos" },
			wantErr: true,
		},
		{
			// Si la lectura falló, la lista no es confiable: no puede venir.
			name: "fuente con error que igual trae elementos",
			mutate: func(r *proto.RuntimeState) {
				r.Sources.Docker = proto.RuntimeSource{Status: proto.SourceError, Detail: "tiempo agotado"}
			},
			wantErr: true,
		},
		{
			name:    "explicación con salto de línea",
			mutate:  func(r *proto.RuntimeState) { r.Sources.Processes.Detail = "uno\ndos" },
			wantErr: true,
		},
		{
			name: "contenedor repetido",
			mutate: func(r *proto.RuntimeState) {
				r.Containers = append(r.Containers, validContainer())
			},
			wantErr: true,
		},
		{
			name:    "servicio repetido",
			mutate:  func(r *proto.RuntimeState) { r.Services = append(r.Services, validService()) },
			wantErr: true,
		},
		{
			name:    "proceso repetido",
			mutate:  func(r *proto.RuntimeState) { r.Processes = append(r.Processes, validProcess()) },
			wantErr: true,
		},
		{
			name:    "demasiados procesos",
			mutate:  func(r *proto.RuntimeState) { r.Processes = manyProcesses(proto.MaxProcesses + 1) },
			wantErr: true,
		},
		{
			name:    "omitidos negativos",
			mutate:  func(r *proto.RuntimeState) { r.Omitted.Services = -1 },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := validRuntimeState()
			tt.mutate(&state)
			err := state.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("se esperaba un error y no hubo")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("no se esperaba error: %v", err)
			}
		})
	}
}

func TestContainerValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*proto.Container)
		wantErr bool
	}{
		{name: "válido", mutate: func(*proto.Container) {}},
		{name: "detenido y sin consumo", mutate: func(c *proto.Container) {
			c.State, c.Health, c.Usage, c.StartedAt = proto.ContainerExited, proto.HealthNone, nil, time.Time{}
		}},
		{name: "sin Compose", mutate: func(c *proto.Container) { c.ComposeProject, c.ComposeService = "", "" }},
		{name: "imagen por digest", mutate: func(c *proto.Container) { c.Image = "postgres@sha256:" + strings.Repeat("0f", 32) }},
		{name: "ID corto", mutate: func(c *proto.Container) { c.ID = "abcdef012345" }, wantErr: true},
		{name: "ID en mayúsculas", mutate: func(c *proto.Container) { c.ID = strings.Repeat("AB", 32) }, wantErr: true},
		{name: "sin nombre", mutate: func(c *proto.Container) { c.Name = "" }, wantErr: true},
		{name: "nombre con barra inicial", mutate: func(c *proto.Container) { c.Name = "/web" }, wantErr: true},
		{name: "nombre con espacio", mutate: func(c *proto.Container) { c.Name = "web 1" }, wantErr: true},
		{name: "imagen con comillas", mutate: func(c *proto.Container) { c.Image = `nginx"; rm` }, wantErr: true},
		{name: "estado inventado", mutate: func(c *proto.Container) { c.State = "zombie" }, wantErr: true},
		{name: "healthcheck inventado", mutate: func(c *proto.Container) { c.Health = "regular" }, wantErr: true},
		{name: "proyecto de Compose con barra", mutate: func(c *proto.Container) { c.ComposeProject = "a/b" }, wantErr: true},
		{name: "CPU imposible", mutate: func(c *proto.Container) { c.Usage.CPUPercent = 101 }, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validContainer()
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("se esperaba un error y no hubo")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("no se esperaba error: %v", err)
			}
		})
	}
}

func TestServiceValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*proto.Service)
		wantErr bool
	}{
		{name: "válido", mutate: func(*proto.Service) {}},
		{name: "instancia de plantilla", mutate: func(s *proto.Service) { s.Unit = "gunicorn@cristales.service" }},
		{name: "nombre con escape de systemd", mutate: func(s *proto.Service) { s.Unit = `srv-app\x2dweb.service` }},
		{name: "caído", mutate: func(s *proto.Service) {
			s.ActiveState, s.SubState, s.MainPID, s.Usage = proto.ActiveFailed, "failed", 0, nil
			s.Result = "oom-kill"
		}},
		{name: "no es un servicio", mutate: func(s *proto.Service) { s.Unit = "nginx.socket" }, wantErr: true},
		{name: "solo el sufijo", mutate: func(s *proto.Service) { s.Unit = ".service" }, wantErr: true},
		{name: "unidad con espacio", mutate: func(s *proto.Service) { s.Unit = "mi app.service" }, wantErr: true},
		{name: "unidad con barra", mutate: func(s *proto.Service) { s.Unit = "../etc.service" }, wantErr: true},
		{name: "estado inventado", mutate: func(s *proto.Service) { s.ActiveState = "dormido" }, wantErr: true},
		{name: "sub_state con mayúsculas", mutate: func(s *proto.Service) { s.SubState = "Running" }, wantErr: true},
		{name: "result con espacio", mutate: func(s *proto.Service) { s.Result = "exit code" }, wantErr: true},
		{name: "result con mayúsculas", mutate: func(s *proto.Service) { s.Result = "Success" }, wantErr: true},
		{name: "sin load_state", mutate: func(s *proto.Service) { s.LoadState = "" }, wantErr: true},
		{name: "PID negativo", mutate: func(s *proto.Service) { s.MainPID = -1 }, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validService()
			tt.mutate(&s)
			err := s.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("se esperaba un error y no hubo")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("no se esperaba error: %v", err)
			}
		})
	}
}

func TestProcessValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*proto.Process)
		wantErr bool
	}{
		{name: "válido", mutate: func(*proto.Process) {}},
		{name: "usuario sin nombre, solo UID", mutate: func(p *proto.Process) { p.User = "1001" }},
		{name: "sin línea de comando (hilo del kernel)", mutate: func(p *proto.Process) { p.Command = "" }},
		{name: "nombre con espacio y acento", mutate: func(p *proto.Process) { p.Name = "trabajo único" }},
		{name: "PID cero", mutate: func(p *proto.Process) { p.PID = 0 }, wantErr: true},
		{name: "PID fuera del kernel", mutate: func(p *proto.Process) { p.PID = 1<<22 + 1 }, wantErr: true},
		{name: "sin nombre", mutate: func(p *proto.Process) { p.Name = "" }, wantErr: true},
		{name: "nombre con escape de terminal", mutate: func(p *proto.Process) { p.Name = "a\x1b[31m" }, wantErr: true},
		{name: "comando con salto de línea", mutate: func(p *proto.Process) { p.Command = "a\nb" }, wantErr: true},
		{name: "comando demasiado largo", mutate: func(p *proto.Process) {
			p.Command = strings.Repeat("x", proto.MaxCommandLen+1)
		}, wantErr: true},
		{name: "UTF-8 roto", mutate: func(p *proto.Process) { p.Name = "a\xffb" }, wantErr: true},
		{name: "sin usuario", mutate: func(p *proto.Process) { p.User = "" }, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validProcess()
			tt.mutate(&p)
			err := p.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("se esperaba un error y no hubo")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("no se esperaba error: %v", err)
			}
		})
	}
}

func TestRuntimeStateAckValidate(t *testing.T) {
	if err := (proto.RuntimeStateAck{TakenAt: time.Now().UTC()}).Validate(); err != nil {
		t.Fatalf("no se esperaba error: %v", err)
	}
	if err := (proto.RuntimeStateAck{}).Validate(); err == nil {
		t.Fatal("un acuse sin marca de tiempo no tiene que pasar")
	}
}

// Un servidor cargado, con todos los topes por tipo al máximo y nombres de
// largo realista, tiene que entrar en un mensaje. Si no entrara, el agent
// tendría que recortar siempre, no solo en casos raros.
func TestLargeRuntimeStateFitsInAMessage(t *testing.T) {
	state := validRuntimeState()
	state.Containers = make([]proto.Container, proto.MaxContainers)
	for i := range state.Containers {
		c := validContainer()
		c.ID = fmt.Sprintf("%064x", i+1)
		c.Name = fmt.Sprintf("cliente-%03d-proyecto-largo-web-1", i)
		c.Image = fmt.Sprintf("registry.example.com/cliente-%03d/proyecto-largo:2026.09.20-abcdef1", i)
		c.ComposeProject = fmt.Sprintf("cliente-%03d-proyecto-largo", i)
		state.Containers[i] = c
	}
	state.Services = make([]proto.Service, proto.MaxServices)
	for i := range state.Services {
		s := validService()
		s.Unit = fmt.Sprintf("gunicorn@cliente-%03d-proyecto-largo.service", i)
		state.Services[i] = s
	}
	state.Processes = manyProcesses(proto.MaxProcesses)
	for i := range state.Processes {
		state.Processes[i].Command = strings.Repeat("x", proto.MaxCommandLen)
	}

	raw, err := proto.Marshal(state)
	if err != nil {
		t.Fatalf("serializar: %v", err)
	}
	if len(raw) > proto.MaxMessageBytes {
		t.Errorf("el estado cargado ocupa %d bytes y el tope es %d", len(raw), proto.MaxMessageBytes)
	}
	t.Logf("el estado cargado ocupa %d bytes de %d", len(raw), proto.MaxMessageBytes)
}

func manyProcesses(n int) []proto.Process {
	processes := make([]proto.Process, n)
	for i := range processes {
		p := validProcess()
		p.PID = 1000 + i
		processes[i] = p
	}
	return processes
}

// Un error largo, con saltos de línea y bytes inválidos, se convierte en una
// fuente que igual pasa el contrato: la foto no se rechaza por un mensaje raro.
func TestSourceFailedFitsTheContract(t *testing.T) {
	err := fmt.Errorf("%s\xff%s", strings.Repeat("línea\n", 100), "fin")
	src := proto.SourceFailed(err)
	if src.Status != proto.SourceError {
		t.Errorf("estado: %q", src.Status)
	}
	if len(src.Detail) > proto.MaxSourceDetailLen {
		t.Errorf("la explicación ocupa %d bytes", len(src.Detail))
	}
	if err := src.Validate(); err != nil {
		t.Errorf("no pasa el contrato: %v", err)
	}
}
