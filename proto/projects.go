package proto

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// Límites de los proyectos declarados (ADR-012). Un servidor tiene unos pocos;
// son techos para lo que se acepta de la red.
const (
	MaxProjects        = 50
	MaxInvalidProjects = 50
	MaxProjectUnits    = 10

	// MaxProjectNameLen es el largo de un nombre de proyecto, que es también
	// el nombre de su archivo.
	MaxProjectNameLen = 63
	// MaxProjectFileLen acota el nombre de un archivo inválido: puede ser
	// cualquier cosa que alguien dejó en el directorio.
	MaxProjectFileLen = 128
	// MaxProjectErrorLen acota la explicación de por qué un archivo es inválido.
	MaxProjectErrorLen = 256

	maxRepoLen          = 256
	maxBranchLen        = 128
	maxPathLen          = 256
	maxURLLen           = 512
	maxHealthDetailLen  = 128
	minHealthTimeoutSec = 1
	maxHealthTimeoutSec = 30
)

// projectNamePattern es el formato de un nombre de proyecto: minúsculas,
// números y guiones, empezando por letra o número. Es seguro como nombre de
// archivo, de directorio y de proyecto de Compose.
var projectNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidProjectName dice si un nombre de proyecto cumple el formato.
func ValidProjectName(name string) bool {
	return projectNamePattern.MatchString(name)
}

// ProjectsState son los proyectos declarados en el servidor, tal como el agent
// los leyó de /etc/status-sv/projects.d (ADR-012). Viaja entero, como
// RuntimeState: la lista reemplaza a la anterior.
//
// Source dice si se pudo leer el directorio. Con unavailable (no existe) la
// lista está vacía y es la verdad; con error, el control plane conserva lo que
// ya tenía.
type ProjectsState struct {
	TakenAt  time.Time        `json:"taken_at"`
	Source   RuntimeSource    `json:"source"`
	Projects []Project        `json:"projects,omitempty"`
	Invalid  []InvalidProject `json:"invalid,omitempty"`
}

// Type implementa Message.
func (ProjectsState) Type() MessageType { return TypeProjectsState }

// Validate implementa Message.
func (p ProjectsState) Validate() error {
	if p.TakenAt.IsZero() {
		return errors.New("falta la marca de tiempo")
	}
	if err := validateSourceList("proyectos", p.Source, len(p.Projects)+len(p.Invalid), MaxProjects+MaxInvalidProjects); err != nil {
		return err
	}
	if len(p.Projects) > MaxProjects {
		return fmt.Errorf("trae %d proyectos y el máximo es %d", len(p.Projects), MaxProjects)
	}
	if len(p.Invalid) > MaxInvalidProjects {
		return fmt.Errorf("trae %d archivos inválidos y el máximo es %d", len(p.Invalid), MaxInvalidProjects)
	}
	names := make(map[string]bool, len(p.Projects))
	for i, project := range p.Projects {
		if err := project.Validate(); err != nil {
			return fmt.Errorf("proyecto %d: %w", i, err)
		}
		if names[project.Name] {
			return fmt.Errorf("proyecto %d: el nombre %s está repetido", i, project.Name)
		}
		names[project.Name] = true
	}
	for i, invalid := range p.Invalid {
		if err := invalid.Validate(); err != nil {
			return fmt.Errorf("archivo inválido %d: %w", i, err)
		}
	}
	return nil
}

// Environment es el entorno de un proyecto.
type Environment string

// Entornos. Producción es la que, en la Fase 3, pide aprobación humana.
const (
	EnvProduction Environment = "production"
	EnvStaging    Environment = "staging"
)

// ProjectRuntime es cómo corre un proyecto.
type ProjectRuntime string

// Runtimes que el agent sabe ligar (ADR-012): Compose por la etiqueta del
// proyecto, systemd por la lista de servicios.
const (
	RuntimeDocker  ProjectRuntime = "docker"
	RuntimeSystemd ProjectRuntime = "systemd"
)

// Project es un proyecto declarado y válido. Viaja lo que el control plane y
// el panel necesitan para mostrarlo y ligarlo con sus unidades; los pasos de
// deploy todavía no existen (Sprint 4).
type Project struct {
	Name        string         `json:"name"`
	Environment Environment    `json:"environment"`
	Repo        string         `json:"repo"`
	Branch      string         `json:"branch"`
	Runtime     ProjectRuntime `json:"runtime"`
	Workdir     string         `json:"workdir"`
	// ComposeFile y ComposeProject van solo con Docker. ComposeProject ya
	// viene resuelto: si el archivo no lo fija, es el directorio de Workdir.
	ComposeFile    string `json:"compose_file,omitempty"`
	ComposeProject string `json:"compose_project,omitempty"`
	// Units va solo con systemd.
	Units       []string         `json:"units,omitempty"`
	Healthcheck *HealthcheckSpec `json:"healthcheck,omitempty"`
	// Health es el último resultado del healthcheck pasivo. Falta si el
	// proyecto no declara healthcheck o si todavía no se probó.
	Health *HealthResult `json:"health,omitempty"`
}

// Validate revisa cada campo. Son las mismas reglas que aplica el parser del
// agent (ADR-012), escritas una sola vez: el agent arma el Project y lo valida
// con esto.
//
// Los errores nombran el campo y la regla, nunca el valor: el agent los manda
// al control plane como explicación de un archivo inválido, y de un archivo
// no viaja su contenido.
func (p Project) Validate() error {
	switch {
	case !ValidProjectName(p.Name):
		return fmt.Errorf("name: tiene que ser de minúsculas, números y guiones, de hasta %d caracteres", MaxProjectNameLen)
	case p.Environment != EnvProduction && p.Environment != EnvStaging:
		return errors.New("environment: tiene que ser production o staging")
	case !validName(p.Repo, maxRepoLen, isRepoRune):
		return errors.New("repo: vacío, demasiado largo o con caracteres no permitidos")
	case !validBranch(p.Branch):
		return errors.New("branch: no es un nombre de rama válido")
	case !validWorkdir(p.Workdir):
		return errors.New("workdir: tiene que ser una ruta absoluta, limpia y que no sea /")
	}

	switch p.Runtime {
	case RuntimeDocker:
		switch {
		case !validRelativePath(p.ComposeFile):
			return errors.New("compose_file: tiene que ser una ruta relativa dentro de workdir")
		case !validName(p.ComposeProject, maxLabelLen, isDockerNameRune):
			return errors.New("compose_project: vacío o con caracteres no permitidos")
		case len(p.Units) > 0:
			return errors.New("units: es solo para proyectos de systemd")
		}
	case RuntimeSystemd:
		switch {
		case p.ComposeFile != "" || p.ComposeProject != "":
			return errors.New("compose_file y compose_project: son solo para proyectos de Docker")
		case len(p.Units) == 0:
			return errors.New("units: un proyecto de systemd necesita al menos un servicio")
		case len(p.Units) > MaxProjectUnits:
			return fmt.Errorf("units: trae %d servicios y el máximo es %d", len(p.Units), MaxProjectUnits)
		}
		seen := make(map[string]bool, len(p.Units))
		for _, unit := range p.Units {
			if !validName(unit, MaxRuntimeNameLen, isUnitRune) || !strings.HasSuffix(unit, ".service") || unit == ".service" {
				return errors.New("units: cada una tiene que ser un servicio válido (terminado en .service)")
			}
			if seen[unit] {
				return errors.New("units: hay un servicio repetido")
			}
			seen[unit] = true
		}
	default:
		return errors.New("runtime: tiene que ser docker o systemd")
	}

	if p.Healthcheck != nil {
		if err := p.Healthcheck.Validate(); err != nil {
			return fmt.Errorf("healthcheck: %w", err)
		}
	}
	if p.Health != nil {
		if p.Healthcheck == nil {
			return errors.New("trae un resultado de healthcheck sin healthcheck declarado")
		}
		if err := p.Health.Validate(); err != nil {
			return fmt.Errorf("resultado del healthcheck: %w", err)
		}
	}
	return nil
}

// HealthcheckSpec es el healthcheck declarado de un proyecto.
type HealthcheckSpec struct {
	URL            string `json:"url"`
	ExpectStatus   int    `json:"expect_status"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// Validate revisa la URL (http o https, sin usuario ni contraseña) y los rangos.
func (h HealthcheckSpec) Validate() error {
	if err := validHealthURL(h.URL); err != nil {
		return err
	}
	if h.ExpectStatus < 100 || h.ExpectStatus > 599 {
		return errors.New("expect_status: tiene que estar entre 100 y 599")
	}
	if h.TimeoutSeconds < minHealthTimeoutSec || h.TimeoutSeconds > maxHealthTimeoutSec {
		return fmt.Errorf("timeout_seconds: tiene que estar entre %d y %d", minHealthTimeoutSec, maxHealthTimeoutSec)
	}
	return nil
}

// HealthState es el resultado de un healthcheck.
type HealthState string

// Resultados del healthcheck pasivo.
const (
	HealthPassing HealthState = "passing"
	HealthFailing HealthState = "failing"
)

// HealthResult es el último healthcheck de un proyecto. StatusCode es 0 si no
// hubo respuesta (timeout, conexión rechazada, certificado inválido), y Detail
// dice qué pasó, sin el cuerpo de la respuesta.
type HealthResult struct {
	State      HealthState `json:"state"`
	CheckedAt  time.Time   `json:"checked_at"`
	StatusCode int         `json:"status_code,omitempty"`
	LatencyMS  int         `json:"latency_ms"`
	Detail     string      `json:"detail,omitempty"`
}

// Validate revisa el estado, los rangos y que el detalle sea texto corto.
func (h HealthResult) Validate() error {
	switch {
	case h.State != HealthPassing && h.State != HealthFailing:
		return fmt.Errorf("estado desconocido %q", h.State)
	case h.CheckedAt.IsZero():
		return errors.New("falta cuándo se probó")
	case h.StatusCode != 0 && (h.StatusCode < 100 || h.StatusCode > 599):
		return fmt.Errorf("código HTTP fuera de rango: %d", h.StatusCode)
	case h.LatencyMS < 0 || h.LatencyMS > maxHealthTimeoutSec*1000*2:
		return fmt.Errorf("latencia fuera de rango: %d ms", h.LatencyMS)
	case len(h.Detail) > maxHealthDetailLen || !isPrintable(h.Detail):
		return errors.New("detalle inválido")
	}
	return nil
}

// InvalidProject es un archivo del directorio que no se pudo usar. Viaja el
// nombre del archivo y qué tiene mal, nunca su contenido.
type InvalidProject struct {
	File  string `json:"file"`
	Error string `json:"error"`
}

// Validate revisa que sean textos cortos e imprimibles.
func (i InvalidProject) Validate() error {
	switch {
	case i.File == "" || len(i.File) > MaxProjectFileLen || !isPrintable(i.File) || strings.ContainsRune(i.File, '/'):
		return fmt.Errorf("nombre de archivo inválido %q", i.File)
	case i.Error == "" || len(i.Error) > MaxProjectErrorLen || !isPrintable(i.Error):
		return errors.New("explicación inválida")
	}
	return nil
}

// ProjectsStateAck confirma que el control plane guardó los proyectos leídos en
// TakenAt.
type ProjectsStateAck struct {
	TakenAt time.Time `json:"taken_at"`
}

// Type implementa Message.
func (ProjectsStateAck) Type() MessageType { return TypeProjectsStateAck }

// Validate implementa Message.
func (a ProjectsStateAck) Validate() error {
	if a.TakenAt.IsZero() {
		return errors.New("falta la marca de tiempo confirmada")
	}
	return nil
}

// isRepoRune cubre direcciones de repositorio como github.com/dueño/repo o
// git@github.com:dueño/repo.git.
func isRepoRune(r rune) bool {
	return isASCIIAlnum(r) || strings.ContainsRune("._-/:@", r)
}

// validBranch sigue lo esencial de git check-ref-format: sin espacios ni
// caracteres especiales, sin "..", sin empezar con "-" ni terminar en "/" o ".lock".
func validBranch(branch string) bool {
	if !validName(branch, maxBranchLen, func(r rune) bool {
		return isASCIIAlnum(r) || strings.ContainsRune("._-/", r)
	}) {
		return false
	}
	return !strings.HasPrefix(branch, "-") && !strings.HasPrefix(branch, "/") &&
		!strings.HasSuffix(branch, "/") && !strings.HasSuffix(branch, ".lock") &&
		!strings.Contains(branch, "..") && !strings.Contains(branch, "//")
}

// isPathRune es lo que se acepta en rutas: nada de espacios, comillas ni
// caracteres que un shell o una plantilla pudieran interpretar.
func isPathRune(r rune) bool {
	return isASCIIAlnum(r) || strings.ContainsRune("._-/", r)
}

func validWorkdir(dir string) bool {
	return validName(dir, maxPathLen, isPathRune) && strings.HasPrefix(dir, "/") &&
		dir != "/" && path.Clean(dir) == dir
}

// validRelativePath acepta una ruta relativa que no sale de su directorio.
func validRelativePath(p string) bool {
	if !validName(p, maxPathLen, isPathRune) || strings.HasPrefix(p, "/") {
		return false
	}
	clean := path.Clean(p)
	return clean == p && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

func validHealthURL(raw string) error {
	if raw == "" || len(raw) > maxURLLen || !isPrintable(raw) || strings.ContainsAny(raw, " \t") {
		return errors.New("url: vacía, demasiado larga o con espacios")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("url: no se puede leer")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("url: tiene que ser http o https")
	case u.Host == "" || u.Hostname() == "":
		return errors.New("url: no tiene servidor")
	case u.User != nil:
		return errors.New("url: no puede llevar usuario ni contraseña")
	case u.Fragment != "":
		return errors.New("url: no puede llevar fragmento")
	}
	return nil
}
