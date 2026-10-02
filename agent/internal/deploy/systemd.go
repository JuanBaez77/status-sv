package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/agent/internal/compose"
	journald "github.com/JuanBaez77/status-sv/agent/internal/journal"
	"github.com/JuanBaez77/status-sv/agent/internal/projectspec"
	"github.com/JuanBaez77/status-sv/agent/internal/runner"
	sd "github.com/JuanBaez77/status-sv/agent/internal/systemd"
	"github.com/JuanBaez77/status-sv/agent/internal/unithelper"
	"github.com/JuanBaez77/status-sv/proto"
)

// journalEvery es cada cuánto se lee del diario la salida de un paso que
// está corriendo.
const journalEvery = 3 * time.Second

// journalTimeout acota la última lectura del diario de un paso, que se hace
// aunque el deploy ya esté cancelado.
const journalTimeout = 10 * time.Second

// Las piezas del deploy de systemd (ADR-019), como interfaces para poder
// probar el motor sin systemd.
type (
	// UnitHelper es el ayudante root: corre los pasos declarados con el
	// usuario del proyecto, y reinicia o para sus unidades. Lo cumple
	// unithelper.Client.
	UnitHelper interface {
		Check(ctx context.Context, project string) error
		RunStep(ctx context.Context, project string, phase unithelper.Phase, index int, deploy uuid.UUID) error
		RestartUnits(ctx context.Context, project string) error
		StopUnits(ctx context.Context, project string) error
	}
	// UnitWatcher lee el estado de una unidad, sin privilegios.
	UnitWatcher interface {
		UnitState(ctx context.Context, unit string) (sd.UnitState, error)
	}
	// StepJournal lee del diario la salida de una unidad. Lo cumple
	// journal.Reader.
	StepJournal interface {
		Tail(ctx context.Context, unit string, lines int) ([]journald.Entry, error)
	}
)

// Systemd son las piezas con las que el motor despliega proyectos de systemd.
type Systemd struct {
	Helper  UnitHelper
	Units   UnitWatcher
	Journal StepJournal
}

// systemdRuntime es un proyecto que corre como servicio de systemd (ADR-019).
// El agent no ejecuta nada del proyecto ni reinicia nada: se lo pide al
// ayudante, que lo saca del project.yaml. Lo que hace el agent es git, leer la
// salida de cada paso del diario y mirar las unidades.
type systemdRuntime struct {
	deps   Systemd
	spec   projectspec.Spec
	deploy uuid.UUID

	mu sync.Mutex
	// seen es el último estado que se vio de cada unidad.
	seen map[string]sd.UnitState
}

func newSystemdRuntime(deps Systemd, spec projectspec.Spec, deploy uuid.UUID) *systemdRuntime {
	return &systemdRuntime{deps: deps, spec: spec, deploy: deploy, seen: make(map[string]sd.UnitState)}
}

// Preflight le pregunta al ayudante lo que solo root puede ver: si el
// usuario, el archivo de entorno y las unidades del proyecto existen.
func (s *systemdRuntime) Preflight(ctx context.Context) (string, error) {
	err := s.deps.Helper.Check(ctx, s.spec.Project.Name)
	var helperErr *unithelper.Error
	switch {
	case errors.Is(err, unithelper.ErrUnavailable):
		return "", deployError(proto.DeployPreflightFailed, "el ayudante de deploy no está instalado en este servidor (install-agent.sh --enable-deploy)")
	case errors.As(err, &helperErr):
		return "", deployError(proto.DeployPreflightFailed, "el proyecto no está listo: %s", helperErr.Message)
	case err != nil:
		return "", deployError(proto.DeployPreflightFailed, "el ayudante de deploy no contestó: %v", err)
	}
	steps := len(s.spec.Build) + len(s.spec.Pre) + len(s.spec.Post)
	return fmt.Sprintf("%s y %s", count(len(s.spec.Project.Units), "unidad", "unidades"), count(steps, "paso declarado", "pasos declarados")), nil
}

// Available es lo mismo que Preflight: lo que revisa el ayudante no depende
// del código del proyecto.
func (s *systemdRuntime) Available(ctx context.Context) (string, error) {
	return s.Preflight(ctx)
}

// Running es la primera unidad del proyecto que no está parada.
func (s *systemdRuntime) Running(ctx context.Context) (string, error) {
	for _, unit := range s.spec.Project.Units {
		state, err := s.deps.Units.UnitState(ctx, unit)
		if err != nil {
			return "", fmt.Errorf("mirar la unidad %s: %w", unit, err)
		}
		if state.Active != "inactive" && state.Active != "failed" {
			return fmt.Sprintf("%s está %s", unit, state.Active), nil
		}
	}
	return "", nil
}

// Down para las unidades declaradas.
func (s *systemdRuntime) Down(ctx context.Context, tail *runner.Tail) error {
	tail.Add("parando " + s.StartLabel())
	err := s.deps.Helper.StopUnits(ctx, s.spec.Project.Name)
	var helperErr *unithelper.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &helperErr) && helperErr.Unit != "":
		return fmt.Errorf("no se pudo parar %s", helperErr.Unit)
	case errors.As(err, &helperErr):
		return fmt.Errorf("no se pudieron parar las unidades: %s", helperErr.Message)
	default:
		return fmt.Errorf("no se pudieron parar las unidades: %w", err)
	}
}

// Un proyecto de systemd no tiene imágenes: a lo que se vuelve es al commit
// anterior, que anota el motor.

func (s *systemdRuntime) Snapshot(context.Context) ([]compose.ServiceImage, error) { return nil, nil }

func (s *systemdRuntime) Describe([]compose.ServiceImage) string { return "" }

func (s *systemdRuntime) Retain(context.Context, string, []compose.ServiceImage) error { return nil }

func (s *systemdRuntime) CanRestore(context.Context, []compose.ServiceImage) error { return nil }

func (s *systemdRuntime) Restore(context.Context, []compose.ServiceImage) error { return nil }

func (s *systemdRuntime) Prune(context.Context, []string) error { return nil }

// Builds son los pasos de deploy.build: lo que prepara el código
// (dependencias, estáticos).
func (s *systemdRuntime) Builds() []build {
	builds := make([]build, 0, len(s.spec.Build))
	for i, step := range s.spec.Build {
		builds = append(builds, build{
			label: commandLine(step),
			run: func(ctx context.Context, tail *runner.Tail) error {
				if err := s.Run(ctx, phaseBuild, i, step, tail); err != nil {
					return stepFailure(err, proto.DeployBuildFailed, commandLine(step)+" falló")
				}
				return nil
			},
		})
	}
	return builds
}

// RebuildsOnRollback es verdadero: lo anterior no está guardado en ningún
// lado, se prepara de nuevo desde el commit anterior.
func (s *systemdRuntime) RebuildsOnRollback() bool { return true }

func (s *systemdRuntime) StartStep() proto.StepName { return proto.StepRestart }

// StartLabel son las unidades que se reinician.
func (s *systemdRuntime) StartLabel() string {
	return strings.Join(s.spec.Project.Units, ", ")
}

func (s *systemdRuntime) Untouched() string {
	return "el código volvió y se preparó de nuevo; el servicio no se había reiniciado"
}

// Run le pide al ayudante que corra un paso declarado y, mientras corre, va
// leyendo su salida del diario.
func (s *systemdRuntime) Run(ctx context.Context, phase string, index int, _ projectspec.Step, tail *runner.Tail) error {
	unit := unithelper.StepUnit(s.deploy.String(), unithelper.Phase(phase), index)
	// La unidad puede tener renglones de antes (el mismo paso, corrido de
	// nuevo al volver atrás): solo interesa lo que salga desde ahora.
	follow := &journalFollower{journal: s.deps.Journal, unit: unit, after: time.Now()}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(journalEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				follow.read(ctx, tail)
			}
		}
	}()
	err := s.deps.Helper.RunStep(ctx, s.spec.Project.Name, unithelper.Phase(phase), index, s.deploy)
	close(stop)
	<-done

	// Lo último que escribió el paso, aunque el deploy ya esté cancelado.
	last, cancel := context.WithTimeout(context.WithoutCancel(ctx), journalTimeout)
	defer cancel()
	follow.read(last, tail)

	return stepError(err)
}

// stepError traduce la respuesta del ayudante a los errores con los que el
// motor arma el mensaje de un paso: el código de salida o el tiempo agotado.
func stepError(err error) error {
	var helperErr *unithelper.Error
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unithelper.ErrUnavailable):
		return errors.New("el ayudante de deploy dejó de estar disponible")
	case errors.As(err, &helperErr):
		switch helperErr.Code {
		case unithelper.CodeTimeout:
			return runner.ErrTimeout
		case unithelper.CodeStepFailed:
			return &runner.ExitError{Code: helperErr.ExitCode}
		default:
			return errors.New(helperErr.Message)
		}
	default:
		return err
	}
}

// Up reinicia las unidades declaradas, en orden.
func (s *systemdRuntime) Up(ctx context.Context, tail *runner.Tail) error {
	// Lo que se había visto de las unidades ya no vale: van a cambiar de
	// proceso, y eso no es que se hayan caído.
	s.mu.Lock()
	clear(s.seen)
	s.mu.Unlock()

	tail.Add("reiniciando " + s.StartLabel())
	err := s.deps.Helper.RestartUnits(ctx, s.spec.Project.Name)
	var helperErr *unithelper.Error
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.As(err, &helperErr) && helperErr.Unit != "":
		return deployError(proto.DeployRestartFailed, "no se pudo reiniciar %s", helperErr.Unit)
	case errors.As(err, &helperErr):
		return deployError(proto.DeployRestartFailed, "no se pudieron reiniciar las unidades: %s", helperErr.Message)
	default:
		return deployError(proto.DeployRestartFailed, "no se pudieron reiniciar las unidades: %v", err)
	}
}

// Check mira una vez las unidades: todas tienen que estar activas y andando,
// con el mismo proceso y sin reinicios nuevos desde la mirada anterior. Un
// servicio que arranca, muere y systemd levanta de nuevo no está bien.
func (s *systemdRuntime) Check(ctx context.Context) (compose.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	problem := ""
	for _, unit := range s.spec.Project.Units {
		state, err := s.deps.Units.UnitState(ctx, unit)
		if err != nil {
			return compose.Status{}, fmt.Errorf("mirar la unidad %s: %w", unit, err)
		}
		previous, seen := s.seen[unit]
		s.seen[unit] = state
		if problem != "" {
			continue
		}
		switch {
		case state.Active != "active" || state.Sub != "running":
			problem = fmt.Sprintf("%s: %s (%s)", unit, state.Active, state.Sub)
		case seen && (state.MainPID != previous.MainPID || state.Restarts != previous.Restarts):
			problem = unit + ": se cayó y systemd la levantó de nuevo"
		}
	}
	if problem != "" {
		return compose.Status{Detail: problem}, nil
	}
	return compose.Status{Ready: true, Detail: count(len(s.spec.Project.Units), "unidad andando", "unidades andando")}, nil
}

// journalFollower lee del diario lo que una unidad va escribiendo y lo pasa
// a la salida del paso, sin repetir renglones entre una lectura y la siguiente.
type journalFollower struct {
	journal StepJournal
	unit    string
	// after es el instante del último renglón que ya se pasó; atAfter,
	// cuántos renglones con ese mismo instante ya se pasaron.
	after   time.Time
	atAfter int
}

func (f *journalFollower) read(ctx context.Context, tail *runner.Tail) {
	entries, err := f.journal.Tail(ctx, f.unit, proto.MaxStepOutputLines)
	if err != nil {
		// Sin diario el paso corre igual: se pierde su salida, no el deploy.
		return
	}
	same := 0
	for _, entry := range entries {
		switch {
		case entry.TS.Before(f.after):
			continue
		case entry.TS.Equal(f.after):
			same++
			if same <= f.atAfter {
				continue
			}
			f.atAfter = same
		default:
			f.after, f.atAfter, same = entry.TS, 1, 1
		}
		tail.Add(entry.Message)
	}
}

// count arma "1 unidad" o "2 unidades".
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
