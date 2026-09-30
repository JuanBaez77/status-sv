package compose

import (
	"context"
	"fmt"
	"strings"

	"github.com/JuanBaez77/status-sv/agent/internal/docker"
)

// Status es cómo están los contenedores del proyecto en un momento de la
// verificación (ADR-015).
type Status struct {
	// Ready es que están todos bien ahora: corriendo (y sanos, si tienen
	// healthcheck), o terminados con código 0 si eran de una sola vez.
	Ready bool
	// Detail lo dice en una línea: cuántos están bien, o el primero que no.
	Detail string
}

// Check mira los contenedores del proyecto. No decide si hay que esperar
// más: eso lo hace la ventana de verificación del motor.
func (p *Project) Check(ctx context.Context, api ImageAPI) (Status, error) {
	containers, err := api.ProjectContainers(ctx, p.name)
	if err != nil {
		return Status{}, err
	}
	if len(containers) == 0 {
		return Status{Detail: "el proyecto no tiene contenedores"}, nil
	}

	ready := 0
	for _, ctr := range containers {
		inspection, err := api.Inspect(ctx, ctr.ID)
		if err != nil {
			return Status{}, err
		}
		if problem := containerProblem(inspection); problem != "" {
			return Status{Detail: name(ctr) + ": " + problem}, nil
		}
		ready++
	}
	return Status{Ready: true, Detail: fmt.Sprintf("%d contenedores bien", ready)}, nil
}

// containerProblem dice qué le falta a un contenedor para estar bien, o nada.
func containerProblem(in docker.Inspection) string {
	state := in.State
	switch state.Status {
	case "running":
		if state.Health == nil || state.Health.Status == "" || state.Health.Status == "none" {
			return ""
		}
		switch state.Health.Status {
		case "healthy":
			return ""
		case "starting":
			return "healthcheck todavía arrancando"
		default:
			return "healthcheck " + state.Health.Status
		}
	case "exited":
		if state.ExitCode == 0 {
			// Un contenedor de una sola vez (una migración en el compose)
			// que terminó bien.
			return ""
		}
		return fmt.Sprintf("terminó con código %d", state.ExitCode)
	case "restarting":
		return "reiniciándose"
	case "":
		return "sin estado"
	default:
		// created, paused, dead, removing.
		return state.Status
	}
}

// name es el nombre del contenedor sin la barra inicial, o su servicio.
func name(ctr docker.Container) string {
	if len(ctr.Names) > 0 {
		return strings.TrimPrefix(ctr.Names[0], "/")
	}
	if s := docker.ServiceOf(ctr); s != "" {
		return s
	}
	return ctr.ID[:min(12, len(ctr.ID))]
}
