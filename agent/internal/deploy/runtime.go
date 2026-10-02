package deploy

import (
	"context"
	"errors"
	"fmt"

	"github.com/JuanBaez77/status-sv/agent/internal/compose"
	"github.com/JuanBaez77/status-sv/agent/internal/projectspec"
	"github.com/JuanBaez77/status-sv/agent/internal/runner"
	"github.com/JuanBaez77/status-sv/proto"
)

// runtime es lo que cambia según cómo corre un proyecto. El motor pone lo
// común: el orden de los pasos, git, el diario, la ventana de verificación y
// la vuelta atrás. Lo que sabe de contenedores, imágenes y Compose queda del
// otro lado de esta interfaz, y el deploy de systemd (ADR-019) va a ser otra
// implementación.
//
// Los métodos devuelven errores con el código del contrato cuando el motivo
// es del runtime ("docker compose build falló"); el motor los pasa tal cual.
type runtime interface {
	// Preflight revisa lo propio del runtime antes de tocar nada. Devuelve el
	// detalle del paso.
	Preflight(ctx context.Context) (string, error)
	// Snapshot anota lo que corre: es a lo que se vuelve si el deploy falla.
	Snapshot(ctx context.Context) ([]compose.ServiceImage, error)
	// Retain guarda lo anotado para que siga existiendo cuando haga falta.
	Retain(ctx context.Context, commit string, images []compose.ServiceImage) error
	// CanRestore dice si todavía se puede volver a lo anotado.
	CanRestore(ctx context.Context, images []compose.ServiceImage) error
	// Build prepara el código nuevo sin tocar lo que corre.
	Build(ctx context.Context, tail *runner.Tail) error
	// Run corre un paso declarado.
	Run(ctx context.Context, step projectspec.Step, tail *runner.Tail) error
	// Up pone a correr lo que quedó preparado.
	Up(ctx context.Context, tail *runner.Tail) error
	// Check mira una vez si lo que corre está bien.
	Check(ctx context.Context) (compose.Status, error)
	// Restore deja preparado lo anotado, para que el próximo Up lo levante.
	Restore(ctx context.Context, images []compose.ServiceImage) error
	// Prune suelta lo guardado de los commits que ya no están en keep.
	Prune(ctx context.Context, keep []string) error
}

// dockerRuntime es un proyecto de Docker Compose (ADR-015): compila y levanta
// con docker compose, y anota, guarda y repone imágenes por la API de Docker.
type dockerRuntime struct {
	project composeProject
	api     compose.ImageAPI
}

func (d dockerRuntime) Preflight(ctx context.Context) (string, error) {
	version, err := d.project.Version(ctx)
	if err != nil {
		return "", deployError(proto.DeployPreflightFailed, "%v", err)
	}
	validate := runner.NewTail(proto.MaxStepOutputLines, proto.MaxStepOutputLineLen)
	if err := d.project.Validate(ctx, validate); err != nil {
		return "", deployError(proto.DeployPreflightFailed, "el archivo de Compose no es válido: %s", lastLine(validate, err))
	}
	return "compose " + version, nil
}

func (d dockerRuntime) Snapshot(ctx context.Context) ([]compose.ServiceImage, error) {
	return d.project.Snapshot(ctx, d.api)
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

func (d dockerRuntime) Build(ctx context.Context, tail *runner.Tail) error {
	if err := d.project.Build(ctx, tail); err != nil {
		return stepFailure(err, proto.DeployBuildFailed, "docker compose build falló")
	}
	return nil
}

func (d dockerRuntime) Run(ctx context.Context, step projectspec.Step, tail *runner.Tail) error {
	return d.project.Run(ctx, step, tail)
}

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

func (d dockerRuntime) Prune(ctx context.Context, keep []string) error {
	return d.project.Prune(ctx, d.api, keep)
}

// runtimeFor arma el runtime de un proyecto declarado.
func (e *Engine) runtimeFor(p proto.Project) (runtime, error) {
	project, err := e.tools.Compose(p)
	if err != nil {
		return nil, err
	}
	return dockerRuntime{project: project, api: e.api}, nil
}
