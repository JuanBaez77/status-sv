// Package runtimestate arma la foto de runtime del servidor con las tres
// fuentes (Docker, systemd y procesos), le suma el consumo de cada unidad y
// decide cuándo mandarla.
//
// La foto viaja entera, nunca como diferencia (proto/README.md). Sale cuando
// algo cambia, con un respiro para juntar ráfagas, y además cada tanto con el
// consumo al día. Si nada cambió, ni siquiera el consumo redondeado, no sale:
// un servidor tranquilo casi no manda nada.
package runtimestate

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/cgroups"
	"github.com/JuanBaez77/status-sv/agent/internal/systemd"
	"github.com/JuanBaez77/status-sv/agent/internal/usage"
	"github.com/JuanBaez77/status-sv/proto"
)

// Las fuentes, como las usa el ensamblador. Las cumplen docker.Source,
// systemd.Source y procs.Scanner; los tests las reemplazan.
type (
	dockerSource interface {
		Snapshot(ctx context.Context) ([]proto.Container, proto.RuntimeSource)
	}
	systemdSource interface {
		Snapshot(ctx context.Context) ([]systemd.Unit, proto.RuntimeSource)
	}
	processSource interface {
		Snapshot(ctx context.Context) ([]proto.Process, int, proto.RuntimeSource)
	}
	usageReader interface {
		Available() bool
		Read(group string) (cgroups.Stat, error)
		ContainerGroup(id string) (string, bool)
	}
)

// Assembler arma fotos. Recuerda el CPU de cada unidad entre fotos y la marca
// de la última, así que no es seguro usarlo desde varias goroutines.
type Assembler struct {
	docker  dockerSource
	systemd systemdSource
	procs   processSource
	cgroups usageReader
	tracker *usage.Tracker
	log     *slog.Logger
	now     func() time.Time
	last    time.Time

	// known son los contenedores y servicios de la última foto. Lo consulta
	// el ejecutor de comandos desde otra goroutine, así que va con candado.
	knownMu sync.Mutex
	known   map[string]bool
}

// NewAssembler arma el ensamblador. cores es la cantidad de núcleos de la
// máquina, para normalizar el CPU de cada unidad al total.
func NewAssembler(d dockerSource, s systemdSource, p processSource, cg usageReader, cores int, log *slog.Logger) *Assembler {
	return &Assembler{
		docker:  d,
		systemd: s,
		procs:   p,
		cgroups: cg,
		tracker: usage.NewTracker(cores),
		log:     log,
		now:     time.Now,
	}
}

// Snapshot toma una foto. Nunca falla: lo que no se pudo leer queda dicho en
// el estado de su fuente, y una unidad que no pasa el contrato se deja afuera
// y se cuenta como omitida, para que una sola no haga rechazar la foto entera.
func (a *Assembler) Snapshot(ctx context.Context) proto.RuntimeState {
	containers, dockerSrc := a.docker.Snapshot(ctx)
	units, systemdSrc := a.systemd.Snapshot(ctx)
	processes, omittedProcs, procsSrc := a.procs.Snapshot(ctx)

	now := a.now()
	state := proto.RuntimeState{
		TakenAt: a.takenAt(now),
		Sources: proto.RuntimeSources{Docker: dockerSrc, Systemd: systemdSrc, Processes: procsSrc},
	}
	measure := a.cgroups.Available()

	for _, c := range containers {
		if measure && isRunningContainer(c.State) {
			if group, ok := a.cgroups.ContainerGroup(c.ID); ok {
				c.Usage = a.usage("c/"+c.ID, group, now)
			}
		}
		if err := c.Validate(); err != nil {
			a.log.WarnContext(ctx, "contenedor que no entra en el contrato; se deja afuera", "contenedor", c.Name, "error", err)
			state.Omitted.Containers++
			continue
		}
		state.Containers = append(state.Containers, c)
	}

	for _, u := range units {
		s := u.Service
		if measure && u.ControlGroup != "" {
			s.Usage = a.usage("s/"+s.Unit, u.ControlGroup, now)
		}
		if err := s.Validate(); err != nil {
			a.log.WarnContext(ctx, "servicio que no entra en el contrato; se deja afuera", "unidad", s.Unit, "error", err)
			state.Omitted.Services++
			continue
		}
		state.Services = append(state.Services, s)
	}

	state.Omitted.Processes = omittedProcs
	for _, p := range processes {
		if err := p.Validate(); err != nil {
			a.log.WarnContext(ctx, "proceso que no entra en el contrato; se deja afuera", "pid", p.PID, "error", err)
			state.Omitted.Processes++
			continue
		}
		state.Processes = append(state.Processes, p)
	}
	a.tracker.Sweep()

	capLists(&state)
	fit(&state)
	a.remember(state)
	return state
}

// Knows dice si el contenedor (por ID) o el servicio (por unidad) estaba en la
// última foto. Es lo que usa el ejecutor de comandos para no operar sobre algo
// que el agent no ve (regla 2).
func (a *Assembler) Knows(kind proto.LogKind, target string) bool {
	a.knownMu.Lock()
	defer a.knownMu.Unlock()
	return a.known[string(kind)+"/"+target]
}

func (a *Assembler) remember(s proto.RuntimeState) {
	known := make(map[string]bool, len(s.Containers)+len(s.Services))
	for _, c := range s.Containers {
		known[string(proto.LogContainer)+"/"+c.ID] = true
	}
	for _, svc := range s.Services {
		known[string(proto.LogService)+"/"+svc.Unit] = true
	}
	a.knownMu.Lock()
	a.known = known
	a.knownMu.Unlock()
}

// takenAt es la marca de la foto: al milisegundo, como las muestras, y siempre
// mayor que la anterior. El control plane descarta una foto que no sea más
// nueva que la guardada, así que dos fotos en el mismo milisegundo perderían
// la segunda.
func (a *Assembler) takenAt(now time.Time) time.Time {
	t := now.UTC().Truncate(time.Millisecond)
	if !t.After(a.last) {
		t = a.last.Add(time.Millisecond)
	}
	a.last = t
	return t
}

// usage mide una unidad. Falta (nil) si no se pudo leer su cgroup o si es la
// primera vez que se la mide: un 0% inventado sería peor que no mostrar nada.
func (a *Assembler) usage(key, group string, now time.Time) *proto.Usage {
	stat, err := a.cgroups.Read(group)
	if err != nil {
		return nil
	}
	percent, ok := a.tracker.CPUPercent(key, stat.CPUUsage, now)
	if !ok {
		return nil
	}
	return &proto.Usage{CPUPercent: percent, MemoryBytes: stat.MemoryBytes}
}

func isRunningContainer(s proto.ContainerState) bool {
	return s == proto.ContainerRunning || s == proto.ContainerPaused || s == proto.ContainerRestarting
}

func isActiveService(s proto.ActiveState) bool {
	return s != proto.ActiveInactive && s != proto.ActiveFailed && s != proto.ActiveUnknown
}

// capLists aplica los topes por tipo del contrato. Si sobra algo, se quedan
// primero los que están corriendo: un contenedor detenido hace años importa
// menos que uno vivo.
func capLists(s *proto.RuntimeState) {
	if extra := len(s.Containers) - proto.MaxContainers; extra > 0 {
		slices.SortStableFunc(s.Containers, func(a, b proto.Container) int {
			return boolOrder(isRunningContainer(a.State), isRunningContainer(b.State))
		})
		s.Containers = s.Containers[:proto.MaxContainers]
		s.Omitted.Containers += extra
	}
	if extra := len(s.Services) - proto.MaxServices; extra > 0 {
		slices.SortStableFunc(s.Services, func(a, b proto.Service) int {
			return boolOrder(isActiveService(a.ActiveState), isActiveService(b.ActiveState))
		})
		s.Services = s.Services[:proto.MaxServices]
		s.Omitted.Services += extra
	}
	if extra := len(s.Processes) - proto.MaxProcesses; extra > 0 {
		s.Processes = s.Processes[:proto.MaxProcesses]
		s.Omitted.Processes += extra
	}
}

// boolOrder pone primero los true.
func boolOrder(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return -1
	default:
		return 1
	}
}

// fit recorta la foto hasta que entre en un mensaje. Con los topes por tipo y
// nombres normales ocupa bastante menos (un test del contrato lo mide), así
// que esto es para el caso raro de nombres enormes: se saca un décimo de la
// lista más larga por vuelta, empezando por el final, que después de capLists
// es lo menos importante.
func fit(s *proto.RuntimeState) {
	for {
		raw, err := proto.Marshal(*s)
		if err != nil || len(raw) <= proto.MaxMessageBytes {
			return
		}
		switch longest := max(len(s.Containers), len(s.Services), len(s.Processes)); {
		case longest == 0:
			return
		case longest == len(s.Services):
			n := max(1, len(s.Services)/10)
			s.Services = s.Services[:len(s.Services)-n]
			s.Omitted.Services += n
		case longest == len(s.Containers):
			n := max(1, len(s.Containers)/10)
			s.Containers = s.Containers[:len(s.Containers)-n]
			s.Omitted.Containers += n
		default:
			n := max(1, len(s.Processes)/10)
			s.Processes = s.Processes[:len(s.Processes)-n]
			s.Omitted.Processes += n
		}
	}
}
