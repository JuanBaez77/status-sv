package projects

// Status es el estado de un proyecto como lo muestra el panel. Se deriva al
// leer, cruzando el proyecto con lo que el agent ve correr (ADR-012): no se
// guarda.
type Status string

// Estados de un proyecto.
const (
	// StatusOK: todo lo que tiene que correr, corre, y el healthcheck (si hay)
	// pasa.
	StatusOK Status = "ok"
	// StatusDegraded: corre algo, pero no todo, o el healthcheck falla.
	StatusDegraded Status = "degraded"
	// StatusDown: no corre nada de lo que tiene que correr.
	StatusDown Status = "down"
	// StatusUnknown: el agent no pudo leer Docker o systemd, así que no se
	// sabe qué corre.
	StatusUnknown Status = "unknown"
)

// Estados de las unidades, con los nombres de Docker y systemd.
const (
	containerRunning = "running"
	containerExited  = "exited"
	containerCreated = "created"
	healthUnhealthy  = "unhealthy"
	healthStarting   = "starting"
	serviceActive    = "active"
	serviceReloading = "reloading"
)

// Container es lo que la regla necesita de un contenedor.
type Container struct {
	State  string
	Health string
}

// Counts dice si un contenedor cuenta para el estado del proyecto y, si
// cuenta, si está bien.
//
// Un contenedor que terminó (exited) o que nunca arrancó (created) no cuenta:
// puede ser una tarea de una sola vez del mismo proyecto de Compose, como las
// migraciones. Lo que falle de verdad lo muestra el healthcheck.
func (c Container) Counts() (counts, healthy bool) {
	if c.State == containerExited || c.State == containerCreated {
		return false, false
	}
	return true, c.State == containerRunning && c.Health != healthUnhealthy && c.Health != healthStarting
}

// ServiceHealthy dice si un servicio de systemd está andando.
func ServiceHealthy(activeState string) bool {
	return activeState == serviceActive || activeState == serviceReloading
}

// Derive calcula el estado de un proyecto a partir de cuántas de sus unidades
// cuentan y cuántas de esas están bien, de si la fuente que las lee está al día
// y del último healthcheck ("" si no hay).
func Derive(sourceOK bool, total, healthy int, health string) Status {
	switch {
	case !sourceOK:
		return StatusUnknown
	case total == 0 || healthy == 0:
		return StatusDown
	case healthy < total || health == "failing":
		return StatusDegraded
	default:
		return StatusOK
	}
}
