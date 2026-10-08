package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/JuanBaez77/status-sv/agent/internal/compose"
	"github.com/JuanBaez77/status-sv/agent/internal/runner"
	"github.com/JuanBaez77/status-sv/internal/projectspec"
	"github.com/JuanBaez77/status-sv/proto"
)

// runtime es lo que cambia según cómo corre un proyecto. El motor pone lo
// común: el orden de los pasos, git, el diario, la ventana de verificación y
// la vuelta atrás. Lo que sabe de contenedores e imágenes (ADR-015) o de
// unidades de systemd y del ayudante root (ADR-019) queda del otro lado de
// esta interfaz.
//
// Los métodos devuelven errores con el código del contrato cuando el motivo
// es del runtime ("docker compose build falló"); el motor los pasa tal cual.
type runtime interface {
	// Preflight revisa lo propio del runtime antes de tocar nada. Devuelve el
	// detalle del paso.
	Preflight(ctx context.Context) (string, error)
	// Available es la parte de Preflight que no necesita el código en el
	// workdir: que el servidor tenga con qué correr el proyecto. Es lo que se
	// puede revisar antes de un primer deploy (ADR-021).
	Available(ctx context.Context) (string, error)
	// Running dice qué hay del proyecto corriendo, o nada. Un primer deploy
	// solo arranca si no hay nada.
	Running(ctx context.Context) (string, error)
	// Down baja lo que corre del proyecto. Es lo que deshace un primer deploy
	// que no quedó bien: no hay nada anterior a lo que volver.
	Down(ctx context.Context, tail *runner.Tail) error
	// Snapshot anota lo que corre: es a lo que se vuelve si el deploy falla.
	Snapshot(ctx context.Context) ([]compose.ServiceImage, error)
	// Describe dice en palabras lo que anotó Snapshot, o nada.
	Describe(images []compose.ServiceImage) string
	// Retain guarda lo anotado para que siga existiendo cuando haga falta.
	Retain(ctx context.Context, commit string, images []compose.ServiceImage) error
	// CanRestore dice si todavía se puede volver a lo anotado.
	CanRestore(ctx context.Context, images []compose.ServiceImage) error
	// Builds son los pasos que preparan el código que está en el workdir,
	// sin tocar lo que corre.
	Builds() []build
	// RebuildsOnRollback dice si para volver atrás hay que correr Builds de
	// nuevo sobre el código anterior. Con Docker no: se vuelve a las imágenes
	// guardadas.
	RebuildsOnRollback() bool
	// Run corre un paso declarado: el número index de la lista phase.
	Run(ctx context.Context, phase string, index int, step projectspec.Step, tail *runner.Tail) error
	// StartStep es el nombre del paso que pone a correr lo preparado.
	StartStep() proto.StepName
	// StartLabel es lo que se muestra de ese paso: qué se pone a correr.
	StartLabel() string
	// Up pone a correr lo que quedó preparado.
	Up(ctx context.Context, tail *runner.Tail) error
	// Check mira una vez si lo que corre está bien.
	Check(ctx context.Context) (compose.Status, error)
	// Restore deja preparado lo anotado, para que el próximo Up lo levante.
	Restore(ctx context.Context, images []compose.ServiceImage) error
	// Untouched es lo que se informa cuando se volvió atrás sin haber llegado
	// a tocar lo que corre.
	Untouched() string
	// Prune suelta lo guardado de los commits que ya no están en keep.
	Prune(ctx context.Context, keep []string) error
}

// build es un paso que prepara el código. label es lo que se muestra de él
// (el comando de un paso declarado), o nada.
type build struct {
	label string
	run   func(ctx context.Context, tail *runner.Tail) error
}

// dockerRuntime es un proyecto de Docker Compose (ADR-015): compila y levanta
// con docker compose, y anota, guarda y repone imágenes por la API de Docker.
type dockerRuntime struct {
	project composeProject
	api     compose.ImageAPI
}

func (d dockerRuntime) Preflight(ctx context.Context) (string, error) {
	detail, err := d.Available(ctx)
	if err != nil {
		return "", err
	}
	validate := runner.NewTail(proto.MaxStepOutputLines, proto.MaxStepOutputLineLen)
	if err := d.project.Validate(ctx, validate); err != nil {
		return "", deployError(proto.DeployPreflightFailed, "el archivo de Compose no es válido: %s", lastLine(validate, err))
	}
	return detail, nil
}

func (d dockerRuntime) Available(ctx context.Context) (string, error) {
	version, err := d.project.Version(ctx)
	if err != nil {
		return "", deployError(proto.DeployPreflightFailed, "%v", err)
	}
	return "compose " + version, nil
}

// Running son los servicios del proyecto que tienen algún contenedor, esté
// andando o parado: con uno solo, el proyecto ya se levantó alguna vez.
func (d dockerRuntime) Running(ctx context.Context) (string, error) {
	images, err := d.project.Snapshot(ctx, d.api)
	if err != nil {
		return "", fmt.Errorf("mirar los contenedores: %w", err)
	}
	if len(images) == 0 {
		return "", nil
	}
	services := make([]string, 0, len(images))
	for _, img := range images {
		services = append(services, img.Service)
	}
	return "contenedores de " + strings.Join(services, ", "), nil
}

// Down saca los contenedores y las redes del proyecto. Los volúmenes y las
// imágenes quedan.
func (d dockerRuntime) Down(ctx context.Context, tail *runner.Tail) error {
	if err := d.project.Down(ctx, tail); err != nil {
		return stepFailure(err, proto.DeployRollbackFailed, "docker compose down falló")
	}
	return nil
}

func (d dockerRuntime) Snapshot(ctx context.Context) ([]compose.ServiceImage, error) {
	return d.project.Snapshot(ctx, d.api)
}

func (d dockerRuntime) Describe(images []compose.ServiceImage) string {
	return count(len(images), "imagen anotada", "imágenes anotadas")
}

func (d dockerRuntime) Retain(ctx context.Context, commit string, images []compose.ServiceImage) error {
	return d.project.Retain(ctx, d.api, commit, images)
}

func (d dockerRuntime) CanRestore(ctx context.Context, images []compose.ServiceImage) error {
	for _, img := range images {
		ok, err := d.api.ImageExists(ctx, img.ID)
		if err != nil {
			return err
		}
		if !ok {
			return deployError(proto.DeployMissingImage, "la imagen anterior de %s ya no existe en el servidor", img.Service)
		}
	}
	return nil
}

func (d dockerRuntime) Builds() []build {
	return []build{{run: func(ctx context.Context, tail *runner.Tail) error {
		if err := d.project.Build(ctx, tail); err != nil {
			return stepFailure(err, proto.DeployBuildFailed, "docker compose build falló")
		}
		return nil
	}}}
}

func (d dockerRuntime) RebuildsOnRollback() bool { return false }

func (d dockerRuntime) Run(ctx context.Context, _ string, _ int, step projectspec.Step, tail *runner.Tail) error {
	return d.project.Run(ctx, step, tail)
}

func (d dockerRuntime) StartStep() proto.StepName { return proto.StepUp }

func (d dockerRuntime) StartLabel() string { return "" }

func (d dockerRuntime) Up(ctx context.Context, tail *runner.Tail) error {
	if err := d.project.Up(ctx, tail); err != nil {
		return stepFailure(err, proto.DeployUpFailed, "docker compose up falló")
	}
	return nil
}

func (d dockerRuntime) Check(ctx context.Context) (compose.Status, error) {
	status, err := d.project.Check(ctx, d.api)
	if err != nil {
		return compose.Status{}, fmt.Errorf("mirar los contenedores: %w", err)
	}
	return status, nil
}

func (d dockerRuntime) Restore(ctx context.Context, images []compose.ServiceImage) error {
	err := d.project.Restore(ctx, d.api, images)
	if errors.Is(err, compose.ErrMissingImage) {
		return deployError(proto.DeployMissingImage, "%v", err)
	}
	return err
}

func (d dockerRuntime) Untouched() string {
	return "el código y las imágenes volvieron; los contenedores no se habían tocado"
}

func (d dockerRuntime) Prune(ctx context.Context, keep []string) error {
	return d.project.Prune(ctx, d.api, keep)
}

// runtimeFor arma el runtime del proyecto de un deploy. spec es la
// declaración con la que arrancó; al retomar un deploy que quedó a medias no
// se tiene, y la de systemd se lee de nuevo: el ayudante corre lo que diga el
// archivo en ese momento.
func (e *Engine) runtimeFor(j *journal, spec *projectspec.Spec) (runtime, error) {
	p := j.Project
	if p.Runtime != proto.RuntimeSystemd {
		project, err := e.tools.Compose(p)
		if err != nil {
			return nil, err
		}
		return dockerRuntime{project: project, api: e.api}, nil
	}

	if e.systemd == nil {
		return nil, errors.New("este agent no puede desplegar proyectos de systemd")
	}
	if spec == nil {
		loaded, err := e.load(e.cfg.ProjectsDir, p.Name)
		if err != nil {
			return nil, fmt.Errorf("leer la declaración de %s: %w", p.Name, err)
		}
		spec = &loaded
	}
	return newSystemdRuntime(*e.systemd, *spec, j.Event.DeployID), nil
}
