package proto

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Límites del estado de runtime. Son techos para lo que se acepta de la red, no
// lo que se espera ver: un VPS típico tiene decenas de contenedores y servicios.
//
// El techo que manda de verdad es MaxMessageBytes. Un estado no se puede partir
// en varios mensajes, así que el agent lo arma para que entre: si no entra,
// deja afuera lo menos importante y lo cuenta en Omitted.
const (
	MaxContainers = 200
	MaxServices   = 300
	MaxProcesses  = 20

	// MaxRuntimeNameLen acota nombres de contenedor, de unidad y de imagen.
	MaxRuntimeNameLen = 256
	// MaxCommandLen acota la línea de comando de un proceso, que el agent
	// recorta y enmascara antes de mandarla.
	MaxCommandLen = 256
	// MaxSourceDetailLen acota la explicación de por qué no se pudo leer una fuente.
	MaxSourceDetailLen = 256

	maxLabelLen    = 128
	maxStateLen    = 32
	maxUserLen     = 64
	maxProcNameLen = 64
	// maxPID es PID_MAX_LIMIT del kernel en 64 bits.
	maxPID = 1 << 22
)

// RuntimeState es la foto completa de lo que corre en el servidor: contenedores
// de Docker, servicios de systemd y los procesos que no maneja ninguno de los
// dos. Viaja entera cada vez, no como diferencia: si se pierde una, la
// siguiente la reemplaza sin que haga falta reconstruir nada.
//
// TakenAt identifica la foto. El control plane descarta una que sea más vieja
// que la que ya tiene guardada, y el acuse la nombra por esa misma marca.
type RuntimeState struct {
	TakenAt    time.Time      `json:"taken_at"`
	Sources    RuntimeSources `json:"sources"`
	Containers []Container    `json:"containers,omitempty"`
	Services   []Service      `json:"services,omitempty"`
	Processes  []Process      `json:"processes,omitempty"`
	Omitted    RuntimeOmitted `json:"omitted,omitzero"`
}

// Type implementa Message.
func (RuntimeState) Type() MessageType { return TypeRuntimeState }

// Validate implementa Message.
func (r RuntimeState) Validate() error {
	if r.TakenAt.IsZero() {
		return errors.New("falta la marca de tiempo de la foto")
	}
	if err := r.Omitted.Validate(); err != nil {
		return fmt.Errorf("omitidos: %w", err)
	}

	if err := validateSourceList("docker", r.Sources.Docker, len(r.Containers), MaxContainers); err != nil {
		return err
	}
	ids := make(map[string]bool, len(r.Containers))
	for i, c := range r.Containers {
		if err := c.Validate(); err != nil {
			return fmt.Errorf("contenedor %d: %w", i, err)
		}
		if ids[c.ID] {
			return fmt.Errorf("contenedor %d: el ID %s está repetido", i, c.ID)
		}
		ids[c.ID] = true
	}

	if err := validateSourceList("systemd", r.Sources.Systemd, len(r.Services), MaxServices); err != nil {
		return err
	}
	units := make(map[string]bool, len(r.Services))
	for i, s := range r.Services {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("servicio %d: %w", i, err)
		}
		if units[s.Unit] {
			return fmt.Errorf("servicio %d: la unidad %s está repetida", i, s.Unit)
		}
		units[s.Unit] = true
	}

	if err := validateSourceList("procesos", r.Sources.Processes, len(r.Processes), MaxProcesses); err != nil {
		return err
	}
	pids := make(map[int]bool, len(r.Processes))
	for i, p := range r.Processes {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("proceso %d: %w", i, err)
		}
		if pids[p.PID] {
			return fmt.Errorf("proceso %d: el PID %d está repetido", i, p.PID)
		}
		pids[p.PID] = true
	}
	return nil
}

// validateSourceList revisa una fuente y la lista que dice haber leído. Una
// fuente que no se leyó bien no puede traer elementos: si los trajera, no se
// sabría si son la lista completa o lo que alcanzó a leer antes de fallar.
func validateSourceList(name string, src RuntimeSource, items, limit int) error {
	if err := src.Validate(); err != nil {
		return fmt.Errorf("fuente %s: %w", name, err)
	}
	if src.Status != SourceOK && items > 0 {
		return fmt.Errorf("fuente %s: está en %q y trae %d elementos", name, src.Status, items)
	}
	if items > limit {
		return fmt.Errorf("fuente %s: trae %d elementos y el máximo es %d", name, items, limit)
	}
	return nil
}

// SourceStatus dice si una fuente se pudo leer. Es lo que le permite al control
// plane distinguir "no hay contenedores" de "no se pudo preguntar".
type SourceStatus string

const (
	// SourceOK indica que la fuente se leyó entera: su lista es la verdad y lo
	// que no aparece dejó de existir.
	SourceOK SourceStatus = "ok"
	// SourceUnavailable indica que la fuente no existe en este servidor, por
	// ejemplo un VPS sin Docker.
	SourceUnavailable SourceStatus = "unavailable"
	// SourceError indica que la fuente existe pero falló al leerla. El control
	// plane conserva lo último que supo en vez de darlo por desaparecido.
	SourceError SourceStatus = "error"
)

// RuntimeSource es el resultado de leer una fuente.
type RuntimeSource struct {
	Status SourceStatus `json:"status"`
	Detail string       `json:"detail,omitempty"`
}

// Validate revisa el estado y el largo de la explicación.
func (s RuntimeSource) Validate() error {
	switch s.Status {
	case SourceOK, SourceUnavailable, SourceError:
	case "":
		return errors.New("falta el estado")
	default:
		return fmt.Errorf("estado desconocido %q", s.Status)
	}
	if len(s.Detail) > MaxSourceDetailLen {
		return fmt.Errorf("la explicación supera %d caracteres", MaxSourceDetailLen)
	}
	if !isPrintable(s.Detail) {
		return errors.New("la explicación tiene caracteres de control")
	}
	return nil
}

// SourceFailed arma el estado de una fuente que existe pero no se pudo leer,
// con el error recortado para que entre en el contrato: sin caracteres de
// control y dentro de MaxSourceDetailLen, sin cortar un carácter por la mitad.
// Un error largo o raro no puede hacer rechazar la foto entera.
func SourceFailed(err error) RuntimeSource {
	msg := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(err.Error(), "?"))
	if len(msg) > MaxSourceDetailLen {
		msg = strings.ToValidUTF8(msg[:MaxSourceDetailLen], "")
	}
	return RuntimeSource{Status: SourceError, Detail: msg}
}

// RuntimeSources es el resultado de cada una de las tres fuentes.
type RuntimeSources struct {
	Docker    RuntimeSource `json:"docker"`
	Systemd   RuntimeSource `json:"systemd"`
	Processes RuntimeSource `json:"processes"`
}

// RuntimeOmitted cuenta lo que quedó afuera de la foto, sea por los topes por
// tipo o porque el mensaje no entraba en MaxMessageBytes.
type RuntimeOmitted struct {
	Containers int `json:"containers,omitempty"`
	Services   int `json:"services,omitempty"`
	Processes  int `json:"processes,omitempty"`
}

// Validate revisa que las cuentas no sean negativas.
func (o RuntimeOmitted) Validate() error {
	if o.Containers < 0 || o.Services < 0 || o.Processes < 0 {
		return fmt.Errorf("las cuentas no pueden ser negativas: %d, %d, %d", o.Containers, o.Services, o.Processes)
	}
	return nil
}

// Usage es lo que consume una unidad en el momento de la foto. Sale de cgroup
// v2 (ADR-011).
//
// CPUPercent está normalizado al total de la máquina, igual que el CPU del
// servidor: un contenedor que ocupa un núcleo entero de cuatro marca 25.
type Usage struct {
	CPUPercent  float64 `json:"cpu_percent"`
	MemoryBytes uint64  `json:"memory_bytes"`
}

// Validate revisa el rango del porcentaje.
func (u Usage) Validate() error {
	if !isPercent(u.CPUPercent) {
		return fmt.Errorf("CPU fuera de rango: %v", u.CPUPercent)
	}
	return nil
}

// ContainerState es el estado de un contenedor, con los nombres de Docker.
type ContainerState string

// Estados de Docker. El adaptador traduce cualquier otro a ContainerUnknown,
// para que un estado nuevo de Docker no haga rechazar la foto entera.
const (
	ContainerCreated    ContainerState = "created"
	ContainerRunning    ContainerState = "running"
	ContainerPaused     ContainerState = "paused"
	ContainerRestarting ContainerState = "restarting"
	ContainerRemoving   ContainerState = "removing"
	ContainerExited     ContainerState = "exited"
	ContainerDead       ContainerState = "dead"
	ContainerUnknown    ContainerState = "unknown"
)

var knownContainerStates = map[ContainerState]bool{
	ContainerCreated: true, ContainerRunning: true, ContainerPaused: true, ContainerRestarting: true,
	ContainerRemoving: true, ContainerExited: true, ContainerDead: true, ContainerUnknown: true,
}

// ContainerHealth es el resultado del healthcheck de Docker. Vacío quiere decir
// que la imagen no declara healthcheck.
type ContainerHealth string

// Resultados del healthcheck, con los nombres de Docker.
const (
	HealthNone      ContainerHealth = ""
	HealthStarting  ContainerHealth = "starting"
	HealthHealthy   ContainerHealth = "healthy"
	HealthUnhealthy ContainerHealth = "unhealthy"
)

var knownHealth = map[ContainerHealth]bool{
	HealthNone: true, HealthStarting: true, HealthHealthy: true, HealthUnhealthy: true,
}

// Container es un contenedor de Docker, corriendo o no.
//
// ComposeProject y ComposeService salen de las etiquetas de Docker Compose y
// son lo que permite, en el Sprint 3, ligar un contenedor a su project.yaml.
// Usage falta si el contenedor no está corriendo o si no se pudo medir.
type Container struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Image          string          `json:"image"`
	State          ContainerState  `json:"state"`
	Health         ContainerHealth `json:"health,omitempty"`
	StartedAt      time.Time       `json:"started_at,omitzero"`
	ComposeProject string          `json:"compose_project,omitempty"`
	ComposeService string          `json:"compose_service,omitempty"`
	Usage          *Usage          `json:"usage,omitempty"`
}

// Validate revisa cada campo contra lo que Docker permite: los nombres llegan
// al panel y, más adelante, a los parámetros de los comandos, así que se
// rechaza cualquier carácter fuera de lo esperado.
func (c Container) Validate() error {
	switch {
	case !isContainerID(c.ID):
		return fmt.Errorf("ID inválido %q: tienen que ser 64 caracteres hexadecimales en minúscula", c.ID)
	case !validName(c.Name, maxLabelLen, isDockerNameRune):
		return fmt.Errorf("nombre inválido %q", c.Name)
	case !validName(c.Image, MaxRuntimeNameLen, isImageRune):
		return fmt.Errorf("imagen inválida %q", c.Image)
	case !knownContainerStates[c.State]:
		return fmt.Errorf("estado desconocido %q", c.State)
	case !knownHealth[c.Health]:
		return fmt.Errorf("healthcheck desconocido %q", c.Health)
	case c.ComposeProject != "" && !validName(c.ComposeProject, maxLabelLen, isDockerNameRune):
		return fmt.Errorf("proyecto de Compose inválido %q", c.ComposeProject)
	case c.ComposeService != "" && !validName(c.ComposeService, maxLabelLen, isDockerNameRune):
		return fmt.Errorf("servicio de Compose inválido %q", c.ComposeService)
	}
	if c.Usage != nil {
		if err := c.Usage.Validate(); err != nil {
			return fmt.Errorf("consumo: %w", err)
		}
	}
	return nil
}

// ActiveState es el estado de una unidad de systemd, con sus nombres.
type ActiveState string

// Estados de systemd. El adaptador traduce cualquier otro a ActiveUnknown.
const (
	ActiveActive       ActiveState = "active"
	ActiveReloading    ActiveState = "reloading"
	ActiveInactive     ActiveState = "inactive"
	ActiveFailed       ActiveState = "failed"
	ActiveActivating   ActiveState = "activating"
	ActiveDeactivating ActiveState = "deactivating"
	ActiveMaintenance  ActiveState = "maintenance"
	ActiveRefreshing   ActiveState = "refreshing"
	ActiveUnknown      ActiveState = "unknown"
)

var knownActiveStates = map[ActiveState]bool{
	ActiveActive: true, ActiveReloading: true, ActiveInactive: true, ActiveFailed: true,
	ActiveActivating: true, ActiveDeactivating: true, ActiveMaintenance: true,
	ActiveRefreshing: true, ActiveUnknown: true,
}

// Service es un servicio de systemd.
//
// LoadState y SubState se pasan como los da systemd, sin lista cerrada: son
// informativos y su conjunto cambia según el tipo de unidad. ActiveState sí es
// una lista cerrada, porque es lo que el panel usa para pintar el estado.
type Service struct {
	Unit        string      `json:"unit"`
	LoadState   string      `json:"load_state"`
	ActiveState ActiveState `json:"active_state"`
	SubState    string      `json:"sub_state"`
	MainPID     int         `json:"main_pid,omitempty"`
	ActiveSince time.Time   `json:"active_since,omitzero"`
	Usage       *Usage      `json:"usage,omitempty"`
}

// Validate revisa el nombre de la unidad con los caracteres que systemd admite
// y que sea un servicio.
func (s Service) Validate() error {
	switch {
	case !validName(s.Unit, MaxRuntimeNameLen, isUnitRune):
		return fmt.Errorf("unidad inválida %q", s.Unit)
	case !strings.HasSuffix(s.Unit, ".service") || s.Unit == ".service":
		return fmt.Errorf("la unidad %q no es un servicio", s.Unit)
	case !validName(s.LoadState, maxStateLen, isStateRune):
		return fmt.Errorf("load_state inválido %q", s.LoadState)
	case !knownActiveStates[s.ActiveState]:
		return fmt.Errorf("estado desconocido %q", s.ActiveState)
	case !validName(s.SubState, maxStateLen, isStateRune):
		return fmt.Errorf("sub_state inválido %q", s.SubState)
	case s.MainPID < 0 || s.MainPID > maxPID:
		return fmt.Errorf("PID principal fuera de rango: %d", s.MainPID)
	}
	if s.Usage != nil {
		if err := s.Usage.Validate(); err != nil {
			return fmt.Errorf("consumo: %w", err)
		}
	}
	return nil
}

// Process es un proceso que no pertenece a ningún contenedor ni servicio: lo
// que corre "a mano" en el servidor. Se muestra en solo lectura.
//
// Command es la línea de comando recortada a MaxCommandLen y con los secretos
// enmascarados por el agent (ADR-011): una contraseña pasada como argumento no
// sale del servidor.
type Process struct {
	PID       int       `json:"pid"`
	Name      string    `json:"name"`
	Command   string    `json:"command,omitempty"`
	User      string    `json:"user"`
	StartedAt time.Time `json:"started_at,omitzero"`
	Usage     Usage     `json:"usage"`
}

// Validate revisa el PID y que los textos no traigan caracteres de control.
func (p Process) Validate() error {
	switch {
	case p.PID <= 0 || p.PID > maxPID:
		return fmt.Errorf("PID fuera de rango: %d", p.PID)
	case p.Name == "":
		return errors.New("falta el nombre")
	case len(p.Name) > maxProcNameLen || !isPrintable(p.Name):
		return fmt.Errorf("nombre inválido %q", p.Name)
	case len(p.Command) > MaxCommandLen || !isPrintable(p.Command):
		return errors.New("línea de comando inválida")
	case !validName(p.User, maxUserLen, isUserRune):
		return fmt.Errorf("usuario inválido %q", p.User)
	}
	if err := p.Usage.Validate(); err != nil {
		return fmt.Errorf("consumo: %w", err)
	}
	return nil
}

// RuntimeStateAck confirma que el control plane guardó la foto tomada en
// TakenAt. El agent la da por entregada y deja de reenviarla.
type RuntimeStateAck struct {
	TakenAt time.Time `json:"taken_at"`
}

// Type implementa Message.
func (RuntimeStateAck) Type() MessageType { return TypeRuntimeStateAck }

// Validate implementa Message.
func (a RuntimeStateAck) Validate() error {
	if a.TakenAt.IsZero() {
		return errors.New("falta la marca de tiempo de la foto confirmada")
	}
	return nil
}

// validName acepta un texto no vacío, de hasta max bytes, formado solo por
// caracteres que cumplen allowed.
func validName(s string, maxLen int, allowed func(rune) bool) bool {
	if s == "" || len(s) > maxLen {
		return false
	}
	for _, r := range s {
		if !allowed(r) {
			return false
		}
	}
	return true
}

// isPrintable acepta texto UTF-8 válido sin caracteres de control. Un salto de
// línea o una secuencia de escape en el nombre de un proceso no tiene por qué
// llegar al panel ni a un log.
func isPrintable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func isContainerID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// isDockerNameRune sigue la regla de Docker para nombres de contenedor
// ([a-zA-Z0-9][a-zA-Z0-9_.-]+), que Compose usa también para proyectos y servicios.
func isDockerNameRune(r rune) bool {
	return isASCIIAlnum(r) || r == '_' || r == '.' || r == '-'
}

// isImageRune cubre referencias de imagen: registro, ruta, etiqueta y digest.
func isImageRune(r rune) bool {
	return isASCIIAlnum(r) || strings.ContainsRune("._-/:@", r)
}

// isUnitRune sigue los caracteres que systemd admite en nombres de unidad,
// incluida la barra invertida de sus secuencias de escape (\x2d).
func isUnitRune(r rune) bool {
	return isASCIIAlnum(r) || strings.ContainsRune(":_.\\@-", r)
}

func isStateRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || r == '-'
}

// isUserRune cubre nombres de usuario de Linux y UIDs numéricos, que es lo que
// queda cuando el usuario no está en /etc/passwd.
func isUserRune(r rune) bool {
	return isASCIIAlnum(r) || r == '_' || r == '.' || r == '-' || r == '$'
}
