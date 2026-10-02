package proto

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Límites del avance de un deploy (ADR-015). Un deploy tiene unos quince pasos
// como mucho; son techos para lo que se acepta de la red. Con salida común, la
// foto más grande entra en un mensaje; si la salida trae muchos caracteres que
// JSON escapa, el agent recorta los renglones más viejos antes de mandarla.
const (
	// MaxDeploySteps cubre los pasos fijos, cinco pre, cinco post y el
	// rollback, con margen.
	MaxDeploySteps = 24
	// MaxStepOutputLines son los últimos renglones de salida que se guardan
	// de cada paso.
	MaxStepOutputLines = 20
	// MaxStepOutputLineLen acota cada renglón de salida, ya enmascarado.
	MaxStepOutputLineLen = 200
	// MaxDeployMessageLen acota la explicación del resultado y el detalle de
	// cada paso.
	MaxDeployMessageLen = 512
	// MaxStepServiceLen es el largo de un nombre de servicio de Compose.
	MaxStepServiceLen = 63
)

// DeployParams es lo que se pide desplegar: un proyecto declarado y, si se
// quiere un commit en particular, su SHA completo. Sin Commit se despliega la
// punta de la rama declarada. El agent verifica que el commit esté en esa rama.
type DeployParams struct {
	Project string `json:"project"`
	Commit  string `json:"commit,omitempty"`
}

// Validate revisa el nombre del proyecto y el SHA.
func (p DeployParams) Validate() error {
	if !ValidProjectName(p.Project) {
		return fmt.Errorf("proyecto inválido %q", p.Project)
	}
	if p.Commit != "" && !ValidCommit(p.Commit) {
		return errors.New("el commit tiene que ser un SHA completo de 40 caracteres hexadecimales en minúscula")
	}
	return nil
}

// RollbackParams es lo que se pide volver atrás: un proyecto declarado, al
// último estado bueno que el agent tiene anotado.
type RollbackParams struct {
	Project string `json:"project"`
}

// Validate revisa el nombre del proyecto.
func (p RollbackParams) Validate() error {
	if !ValidProjectName(p.Project) {
		return fmt.Errorf("proyecto inválido %q", p.Project)
	}
	return nil
}

// ValidCommit dice si s es un SHA-1 completo de git, en minúscula. No se
// aceptan abreviaturas ni nombres de ramas: lo que se despliega es exacto.
func ValidCommit(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// DeployKind dice si el deploy trae código nuevo o vuelve al último estado bueno.
type DeployKind string

// Tipos de deploy.
const (
	DeployKindDeploy   DeployKind = "deploy"
	DeployKindRollback DeployKind = "rollback"
)

// DeployStatus es cómo va un deploy. Los tres últimos son finales.
type DeployStatus string

// Estados de un deploy.
const (
	DeployRunning DeployStatus = "running"
	// DeploySucceeded es que todos los pasos terminaron bien.
	DeploySucceeded DeployStatus = "succeeded"
	// DeployFailed es que falló sin cambiar nada de lo que corre, o que
	// falló y además no pudo volver atrás (código rollback_failed).
	DeployFailed DeployStatus = "failed"
	// DeployRolledBack es que falló después de tocar lo que corre y volvió
	// atrás bien: el proyecto quedó como estaba.
	DeployRolledBack DeployStatus = "rolled_back"
)

// Final dice si el deploy terminó.
func (s DeployStatus) Final() bool {
	return s == DeploySucceeded || s == DeployFailed || s == DeployRolledBack
}

// DeployErrorCode dice por qué un deploy no terminó bien. Es una lista
// cerrada, como los códigos de los comandos: el panel decide qué mostrar por
// el código, no por el texto.
type DeployErrorCode string

// Códigos de un deploy que no terminó bien.
const (
	// DeployPreflightFailed: el workdir, el repo, el archivo de Compose o la
	// llave no están como tienen que estar.
	DeployPreflightFailed DeployErrorCode = "preflight_failed"
	// DeployDirtyWorktree: alguien cambió archivos versionados a mano.
	DeployDirtyWorktree DeployErrorCode = "dirty_worktree"
	// DeployFetchFailed: no se pudo traer la rama declarada.
	DeployFetchFailed DeployErrorCode = "fetch_failed"
	// DeployCommitNotOnBranch: el commit pedido no está en la rama declarada.
	DeployCommitNotOnBranch DeployErrorCode = "commit_not_on_branch"
	// DeployBuildFailed: docker compose build falló.
	DeployBuildFailed DeployErrorCode = "build_failed"
	// DeployStepFailed: un paso declarado (pre o post) falló.
	DeployStepFailed DeployErrorCode = "step_failed"
	// DeployUpFailed: docker compose up falló.
	DeployUpFailed DeployErrorCode = "up_failed"
	// DeployRestartFailed: no se pudieron reiniciar las unidades de un
	// proyecto systemd (ADR-019).
	DeployRestartFailed DeployErrorCode = "restart_failed"
	// DeployUnhealthy: la verificación no pasó a tiempo.
	DeployUnhealthy DeployErrorCode = "unhealthy"
	// DeployTimeout: el deploy entero se pasó de su tiempo máximo.
	DeployTimeout DeployErrorCode = "timeout"
	// DeployInterrupted: el agent se reinició a mitad del deploy.
	DeployInterrupted DeployErrorCode = "interrupted"
	// DeployNoPrevious: se pidió volver atrás y no hay un estado bueno
	// anotado.
	DeployNoPrevious DeployErrorCode = "no_previous"
	// DeployMissingImage: la imagen a la que había que volver ya no existe.
	DeployMissingImage DeployErrorCode = "missing_image"
	// DeployRollbackFailed: falló y tampoco pudo volver atrás. Es el único
	// caso en que el proyecto puede quedar a medias: hay que mirarlo.
	DeployRollbackFailed DeployErrorCode = "rollback_failed"
	// DeployInternal: cualquier otra cosa.
	DeployInternal DeployErrorCode = "internal"
)

var knownDeployCodes = map[DeployErrorCode]bool{
	DeployPreflightFailed: true, DeployDirtyWorktree: true, DeployFetchFailed: true,
	DeployCommitNotOnBranch: true, DeployBuildFailed: true, DeployStepFailed: true,
	DeployUpFailed: true, DeployRestartFailed: true, DeployUnhealthy: true, DeployTimeout: true, DeployInterrupted: true,
	DeployNoPrevious: true, DeployMissingImage: true, DeployRollbackFailed: true, DeployInternal: true,
}

// StepName es un paso del deploy, en el orden de ADR-015. Un proyecto systemd
// (ADR-019) usa los mismos, con restart en lugar de up, y puede traer más de
// un build: uno por cada paso declarado en deploy.build.
type StepName string

// Pasos de un deploy.
const (
	StepPreflight StepName = "preflight"
	StepFetch     StepName = "fetch"
	StepSnapshot  StepName = "snapshot"
	StepCheckout  StepName = "checkout"
	StepBuild     StepName = "build"
	StepPre       StepName = "pre"
	StepUp        StepName = "up"
	// StepRestart reinicia las unidades de un proyecto systemd.
	StepRestart     StepName = "restart"
	StepHealthcheck StepName = "healthcheck"
	StepPost        StepName = "post"
	// StepRollback es la vuelta atrás, cuando algo falló después de tocar lo
	// que corre.
	StepRollback StepName = "rollback"
)

var knownSteps = map[StepName]bool{
	StepPreflight: true, StepFetch: true, StepSnapshot: true, StepCheckout: true, StepBuild: true,
	StepPre: true, StepUp: true, StepRestart: true, StepHealthcheck: true, StepPost: true, StepRollback: true,
}

// StepStatus es cómo va un paso.
type StepStatus string

// Estados de un paso.
const (
	StepRunning StepStatus = "running"
	StepOK      StepStatus = "ok"
	StepFailed  StepStatus = "failed"
	// StepSkipped es un paso que no hizo falta (el build de un rollback, por
	// ejemplo).
	StepSkipped StepStatus = "skipped"
)

// DeployEvent es la foto completa de un deploy, tal como la tiene el agent.
// Viaja entera cada vez que cambia algo: si se pierde una, la siguiente la
// reemplaza (ADR-015). Seq crece con cada foto del mismo deploy, y el control
// plane ignora una más vieja que la que ya guardó.
//
// DeployID es el ID del comando que lo pidió.
type DeployEvent struct {
	DeployID uuid.UUID    `json:"deploy_id"`
	Seq      int          `json:"seq"`
	Project  string       `json:"project"`
	Kind     DeployKind   `json:"kind"`
	Status   DeployStatus `json:"status"`
	// Commit es el SHA que se despliega, apenas se conoce; PreviousCommit, el
	// que había antes.
	Commit         string          `json:"commit,omitempty"`
	PreviousCommit string          `json:"previous_commit,omitempty"`
	StartedAt      time.Time       `json:"started_at"`
	FinishedAt     time.Time       `json:"finished_at,omitzero"`
	Code           DeployErrorCode `json:"code,omitempty"`
	Message        string          `json:"message,omitempty"`
	Steps          []DeployStep    `json:"steps,omitempty"`
}

// Type implementa Message.
func (DeployEvent) Type() MessageType { return TypeDeployEvent }

// Validate implementa Message.
func (e DeployEvent) Validate() error {
	switch {
	case e.DeployID == uuid.Nil:
		return errors.New("falta el ID del deploy")
	case e.Seq < 1:
		return errors.New("la secuencia empieza en 1")
	case !ValidProjectName(e.Project):
		return fmt.Errorf("proyecto inválido %q", e.Project)
	case e.Kind != DeployKindDeploy && e.Kind != DeployKindRollback:
		return fmt.Errorf("tipo de deploy desconocido %q", e.Kind)
	case e.Commit != "" && !ValidCommit(e.Commit):
		return errors.New("commit inválido")
	case e.PreviousCommit != "" && !ValidCommit(e.PreviousCommit):
		return errors.New("commit anterior inválido")
	case e.StartedAt.IsZero():
		return errors.New("falta cuándo empezó")
	case len(e.Message) > MaxDeployMessageLen || !isPrintable(e.Message):
		return errors.New("la explicación es demasiado larga o tiene caracteres de control")
	case len(e.Steps) > MaxDeploySteps:
		return fmt.Errorf("trae %d pasos y el máximo es %d", len(e.Steps), MaxDeploySteps)
	}

	switch e.Status {
	case DeployRunning:
		if !e.FinishedAt.IsZero() {
			return errors.New("un deploy en curso no tiene fin")
		}
		if e.Code != "" {
			return errors.New("un deploy en curso no lleva código de error")
		}
	case DeploySucceeded:
		if e.FinishedAt.IsZero() {
			return errors.New("falta cuándo terminó")
		}
		if e.Code != "" {
			return errors.New("un deploy que terminó bien no lleva código de error")
		}
	case DeployFailed, DeployRolledBack:
		if e.FinishedAt.IsZero() {
			return errors.New("falta cuándo terminó")
		}
		if !knownDeployCodes[e.Code] {
			return fmt.Errorf("código de error desconocido %q", e.Code)
		}
	default:
		return fmt.Errorf("estado desconocido %q", e.Status)
	}
	if !e.FinishedAt.IsZero() && e.FinishedAt.Before(e.StartedAt) {
		return errors.New("termina antes de empezar")
	}

	for i, step := range e.Steps {
		if err := step.Validate(); err != nil {
			return fmt.Errorf("paso %d: %w", i, err)
		}
	}
	return nil
}

// DeployStep es un paso del deploy con su resultado. Service va solo en los
// pasos declarados (pre y post) de un proyecto Docker: es el servicio de
// Compose donde corrió. Los de un proyecto systemd no lo llevan (ADR-019).
type DeployStep struct {
	Name       StepName   `json:"name"`
	Service    string     `json:"service,omitempty"`
	Status     StepStatus `json:"status"`
	StartedAt  time.Time  `json:"started_at,omitzero"`
	FinishedAt time.Time  `json:"finished_at,omitzero"`
	// Detail explica el resultado en una línea ("3 contenedores sanos",
	// "salió con código 1").
	Detail string `json:"detail,omitempty"`
	// Output son los últimos renglones de salida del paso, enmascarados.
	Output []string `json:"output,omitempty"`
}

// Validate revisa el nombre, el estado, los tiempos y la salida.
func (s DeployStep) Validate() error {
	switch {
	case !knownSteps[s.Name]:
		return fmt.Errorf("paso desconocido %q", s.Name)
	case s.Service != "" && s.Name != StepPre && s.Name != StepPost:
		return errors.New("solo los pasos declarados llevan servicio")
	case s.Service != "" && !validName(s.Service, MaxStepServiceLen, isDockerNameRune):
		return errors.New("el servicio de un paso declarado no es un nombre válido")
	case s.Status != StepRunning && s.Status != StepOK && s.Status != StepFailed && s.Status != StepSkipped:
		return fmt.Errorf("estado de paso desconocido %q", s.Status)
	case s.Status == StepRunning && !s.FinishedAt.IsZero():
		return errors.New("un paso en curso no tiene fin")
	case !s.StartedAt.IsZero() && !s.FinishedAt.IsZero() && s.FinishedAt.Before(s.StartedAt):
		return errors.New("termina antes de empezar")
	case len(s.Detail) > MaxDeployMessageLen || !isPrintable(s.Detail):
		return errors.New("detalle demasiado largo o con caracteres de control")
	case len(s.Output) > MaxStepOutputLines:
		return fmt.Errorf("trae %d renglones de salida y el máximo es %d", len(s.Output), MaxStepOutputLines)
	}
	for i, line := range s.Output {
		if len(line) > MaxStepOutputLineLen || !isPrintable(line) {
			return fmt.Errorf("renglón de salida %d demasiado largo o con caracteres de control", i)
		}
	}
	return nil
}

// DeployEventAck confirma que el control plane guardó la foto Seq del deploy
// DeployID. Con el acuse de una foto final, el agent la da por entregada.
type DeployEventAck struct {
	DeployID uuid.UUID `json:"deploy_id"`
	Seq      int       `json:"seq"`
}

// Type implementa Message.
func (DeployEventAck) Type() MessageType { return TypeDeployEventAck }

// Validate implementa Message.
func (a DeployEventAck) Validate() error {
	if a.DeployID == uuid.Nil {
		return errors.New("falta el ID del deploy")
	}
	if a.Seq < 1 {
		return errors.New("la secuencia empieza en 1")
	}
	return nil
}
