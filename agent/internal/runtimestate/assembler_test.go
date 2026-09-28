package runtimestate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/cgroups"
	"github.com/JuanBaez77/status-sv/agent/internal/systemd"
	"github.com/JuanBaez77/status-sv/proto"
)

var (
	ok = proto.RuntimeSource{Status: proto.SourceOK}
	t0 = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
)

type fakeDocker struct {
	containers []proto.Container
	src        proto.RuntimeSource
}

func (f *fakeDocker) Snapshot(context.Context) ([]proto.Container, proto.RuntimeSource) {
	return f.containers, f.src
}

type fakeSystemd struct {
	units []systemd.Unit
	src   proto.RuntimeSource
}

func (f *fakeSystemd) Snapshot(context.Context) ([]systemd.Unit, proto.RuntimeSource) {
	return f.units, f.src
}

type fakeProcs struct {
	processes []proto.Process
	omitted   int
	src       proto.RuntimeSource
}

func (f *fakeProcs) Snapshot(context.Context) ([]proto.Process, int, proto.RuntimeSource) {
	return f.processes, f.omitted, f.src
}

// fakeCgroups devuelve el consumo que se le cargue por cgroup.
type fakeCgroups struct {
	available bool
	stats     map[string]cgroups.Stat
}

func (f *fakeCgroups) Available() bool { return f.available }

func (f *fakeCgroups) Read(group string) (cgroups.Stat, error) {
	st, ok := f.stats[group]
	if !ok {
		return cgroups.Stat{}, cgroups.ErrNotFound
	}
	return st, nil
}

func (f *fakeCgroups) ContainerGroup(id string) (string, bool) {
	group := "/system.slice/docker-" + id + ".scope"
	_, ok := f.stats[group]
	return group, ok
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func containerID(n int) string { return fmt.Sprintf("%064x", n) }

type fixture struct {
	docker  *fakeDocker
	systemd *fakeSystemd
	procs   *fakeProcs
	cgroups *fakeCgroups
	now     time.Time
	a       *Assembler
}

func newFixture() *fixture {
	f := &fixture{
		docker: &fakeDocker{src: ok, containers: []proto.Container{
			{ID: containerID(1), Name: "web", Image: "nginx:1.27", State: proto.ContainerRunning},
			{ID: containerID(2), Name: "viejo", Image: "busybox", State: proto.ContainerExited},
		}},
		systemd: &fakeSystemd{src: ok, units: []systemd.Unit{{
			Service:      proto.Service{Unit: "nginx.service", LoadState: "loaded", ActiveState: proto.ActiveActive, SubState: "running"},
			ControlGroup: "/system.slice/nginx.service",
		}}},
		procs: &fakeProcs{src: ok, omitted: 2, processes: []proto.Process{{PID: 900, Name: "python3", User: "juan"}}},
		cgroups: &fakeCgroups{available: true, stats: map[string]cgroups.Stat{
			"/system.slice/docker-" + containerID(1) + ".scope": {CPUUsage: time.Minute, MemoryBytes: 50 << 20},
			"/system.slice/nginx.service":                       {CPUUsage: time.Minute, MemoryBytes: 10 << 20},
		}},
		now: t0,
	}
	f.a = NewAssembler(f.docker, f.systemd, f.procs, f.cgroups, 4, quietLog())
	f.a.now = func() time.Time { return f.now }
	return f
}

func TestSnapshotJoinsTheThreeSources(t *testing.T) {
	f := newFixture()
	s := f.a.Snapshot(context.Background())

	if len(s.Containers) != 2 || len(s.Services) != 1 || len(s.Processes) != 1 {
		t.Fatalf("foto: %d contenedores, %d servicios, %d procesos", len(s.Containers), len(s.Services), len(s.Processes))
	}
	if s.Omitted.Processes != 2 {
		t.Errorf("los omitidos por el escáner se conservan: %+v", s.Omitted)
	}
	if !s.TakenAt.Equal(t0) {
		t.Errorf("marca: %v", s.TakenAt)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("la foto no pasa el contrato: %v", err)
	}
}

// La primera foto no tiene contra qué comparar el CPU: sin consumo, en vez de
// un 0% inventado. La segunda ya lo trae.
func TestUsageNeedsTwoReadings(t *testing.T) {
	f := newFixture()
	first := f.a.Snapshot(context.Background())
	if first.Containers[0].Usage != nil || first.Services[0].Usage != nil {
		t.Fatal("la primera foto no puede traer consumo")
	}

	f.now = t0.Add(10 * time.Second)
	f.cgroups.stats["/system.slice/docker-"+containerID(1)+".scope"] = cgroups.Stat{CPUUsage: time.Minute + 10*time.Second, MemoryBytes: 60 << 20}
	f.cgroups.stats["/system.slice/nginx.service"] = cgroups.Stat{CPUUsage: time.Minute, MemoryBytes: 10 << 20}
	second := f.a.Snapshot(context.Background())

	web := second.Containers[0].Usage
	if web == nil || web.CPUPercent != 25 || web.MemoryBytes != 60<<20 {
		t.Errorf("consumo de web: %+v (un núcleo de cuatro es 25%%)", web)
	}
	if u := second.Services[0].Usage; u == nil || u.CPUPercent != 0 {
		t.Errorf("consumo de nginx: %+v", u)
	}
	// Un contenedor detenido no se mide.
	if second.Containers[1].Usage != nil {
		t.Error("un contenedor detenido no tiene consumo")
	}
}

func TestNoUsageWithoutCgroupV2(t *testing.T) {
	f := newFixture()
	f.cgroups.available = false
	f.a.Snapshot(context.Background())
	f.now = t0.Add(10 * time.Second)
	s := f.a.Snapshot(context.Background())
	if s.Containers[0].Usage != nil {
		t.Error("sin cgroup v2 no hay consumo")
	}
}

// Una unidad que no pasa el contrato se deja afuera y se cuenta, en vez de
// hacer rechazar la foto entera.
func TestInvalidUnitsAreOmitted(t *testing.T) {
	f := newFixture()
	f.docker.containers = append(f.docker.containers, proto.Container{ID: "corto", Name: "raro", Image: "x", State: proto.ContainerRunning})
	f.systemd.units = append(f.systemd.units, systemd.Unit{Service: proto.Service{Unit: "sin sufijo", LoadState: "loaded", ActiveState: proto.ActiveActive, SubState: "running"}})

	s := f.a.Snapshot(context.Background())
	if s.Omitted.Containers != 1 || s.Omitted.Services != 1 {
		t.Errorf("omitidos: %+v", s.Omitted)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("la foto no pasa el contrato: %v", err)
	}
}

func TestFailedSourcesPassThrough(t *testing.T) {
	f := newFixture()
	f.docker.containers, f.docker.src = nil, proto.SourceFailed(errors.New("tiempo agotado"))
	s := f.a.Snapshot(context.Background())
	if s.Sources.Docker.Status != proto.SourceError || len(s.Containers) != 0 {
		t.Errorf("fuente de Docker: %+v con %d contenedores", s.Sources.Docker, len(s.Containers))
	}
	if err := s.Validate(); err != nil {
		t.Errorf("la foto no pasa el contrato: %v", err)
	}
}

// Dos fotos en el mismo milisegundo no pueden tener la misma marca: el control
// plane descartaría la segunda.
func TestTakenAtAlwaysAdvances(t *testing.T) {
	f := newFixture()
	a := f.a.Snapshot(context.Background())
	b := f.a.Snapshot(context.Background())
	if !b.TakenAt.After(a.TakenAt) {
		t.Errorf("la segunda marca (%v) no es posterior a la primera (%v)", b.TakenAt, a.TakenAt)
	}
}

// Con más contenedores que el tope, quedan primero los que corren.
func TestCapKeepsRunningFirst(t *testing.T) {
	f := newFixture()
	f.docker.containers = nil
	for i := range proto.MaxContainers + 10 {
		state := proto.ContainerExited
		if i >= proto.MaxContainers {
			state = proto.ContainerRunning // los últimos, pero vivos
		}
		f.docker.containers = append(f.docker.containers, proto.Container{
			ID: containerID(i + 1), Name: fmt.Sprintf("c%d", i), Image: "x", State: state,
		})
	}
	s := f.a.Snapshot(context.Background())
	if len(s.Containers) != proto.MaxContainers || s.Omitted.Containers != 10 {
		t.Fatalf("%d contenedores y %d omitidos", len(s.Containers), s.Omitted.Containers)
	}
	for _, c := range s.Containers[:10] {
		if c.State != proto.ContainerRunning {
			t.Fatal("los que corren tienen que quedar")
		}
	}
}

// Si con nombres enormes la foto no entra en un mensaje, se recorta hasta que
// entre, y lo recortado se cuenta.
func TestFitShrinksOversizedSnapshots(t *testing.T) {
	s := proto.RuntimeState{TakenAt: t0, Sources: proto.RuntimeSources{Docker: ok, Systemd: ok, Processes: ok}}
	// La barra invertida es válida en un nombre de unidad (systemd la usa para
	// escapar) y JSON la duplica: es la foto más pesada que admite el contrato.
	long := strings.Repeat(`\`, proto.MaxRuntimeNameLen-len(".service"))
	for i := range proto.MaxServices {
		s.Services = append(s.Services, proto.Service{
			Unit:      fmt.Sprintf("%s%03d.service", long[:len(long)-3], i),
			LoadState: "loaded", ActiveState: proto.ActiveActive, SubState: "running",
		})
	}
	for i := range proto.MaxContainers {
		s.Containers = append(s.Containers, proto.Container{
			ID: containerID(i + 1), Name: fmt.Sprintf("%s%03d", strings.Repeat("n", 120), i),
			Image: strings.Repeat("i", proto.MaxRuntimeNameLen), State: proto.ContainerRunning,
		})
	}
	before := len(s.Services) + len(s.Containers)

	fit(&s)
	raw, err := proto.Marshal(s)
	if err != nil {
		t.Fatalf("serializar: %v", err)
	}
	if len(raw) > proto.MaxMessageBytes {
		t.Errorf("la foto ocupa %d bytes", len(raw))
	}
	after := len(s.Services) + len(s.Containers)
	if s.Omitted.Services+s.Omitted.Containers != before-after || after == before {
		t.Errorf("recortadas %d, contadas %d", before-after, s.Omitted.Services+s.Omitted.Containers)
	}
}
