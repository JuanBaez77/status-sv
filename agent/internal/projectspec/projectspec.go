// Package projectspec lee los project.yaml que una persona declara en el
// servidor, en /etc/status-sv/projects.d (ADR-012).
//
// El parser es estricto: un archivo con cualquier error queda afuera entero,
// con una explicación que nombra el campo y la regla pero nunca el valor, porque
// esa explicación viaja al control plane y de un archivo no sale su contenido.
//
// Las reglas de cada campo están en proto.Project.Validate: el parser arma el
// proyecto y lo valida con eso, así la regla se escribe una sola vez. Lo que
// agrega acá es lo que solo tiene sentido sobre el archivo: el tamaño, los
// campos desconocidos, que el nombre coincida con el del archivo y los pasos
// declarados del deploy (ADR-016), que no viajan al control plane.
package projectspec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/JuanBaez77/status-sv/proto"
)

// MaxFileBytes es el tamaño máximo de un project.yaml. Uno real ocupa menos de
// 1 KiB; más que esto es un error o un abuso.
const MaxFileBytes = 64 << 10

// Valores por defecto del healthcheck, cuando el archivo no los fija.
const (
	DefaultExpectStatus   = 200
	DefaultTimeoutSeconds = 10
)

// RollbackPreviousImage es el único rollback declarable hoy: con Docker,
// volver a la imagen anterior. El de systemd llega en el Sprint 5.
const RollbackPreviousImage = "previous_image"

// ErrNotProjectFile es un archivo del directorio que no se lee como proyecto:
// no termina en .yaml ni en .yml. Se ignora en silencio, como el README o un
// respaldo que alguien dejó.
var ErrNotProjectFile = errors.New("no es un archivo de proyecto")

// file es el project.yaml tal como se escribe. Con KnownFields, cualquier campo
// que no esté acá es un error: un typo no puede pasar desapercibido.
type file struct {
	Name           string       `yaml:"name"`
	Environment    string       `yaml:"environment"`
	Repo           string       `yaml:"repo"`
	Branch         string       `yaml:"branch"`
	Runtime        string       `yaml:"runtime"`
	ComposeFile    string       `yaml:"compose_file"`
	ComposeProject string       `yaml:"compose_project"`
	Workdir        string       `yaml:"workdir"`
	Units          []string     `yaml:"units"`
	Healthcheck    *healthcheck `yaml:"healthcheck"`
	Deploy         *deploySteps `yaml:"deploy"`
	Rollback       string       `yaml:"rollback"`
}

type healthcheck struct {
	URL            string `yaml:"url"`
	ExpectStatus   int    `yaml:"expect_status"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

// deploySteps son los pasos previos y posteriores del deploy (ADR-016).
type deploySteps struct {
	Pre  []step `yaml:"pre"`
	Post []step `yaml:"post"`
}

// step es un paso declarado tal como se escribe.
type step struct {
	Service        string   `yaml:"service"`
	Command        []string `yaml:"command"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
}

// Límites de los pasos declarados (ADR-016).
const (
	MaxSteps            = 5
	MaxStepArgs         = 32
	MaxStepArgLen       = 256
	DefaultStepTimeout  = 300 * time.Second
	maxStepTimeoutSecs  = 1800
	maxStepServiceChars = 63
)

// Spec es un proyecto declarado con lo que solo usa el agent: los pasos del
// deploy. Project es lo que viaja al control plane; los pasos no, porque un
// comando podría traer algo que no tiene por qué salir del servidor.
type Spec struct {
	Project proto.Project
	Pre     []Step
	Post    []Step
}

// Step es un paso declarado: un comando que corre con docker compose run en
// un servicio del proyecto, con la imagen recién compilada.
type Step struct {
	Service string
	Command []string
	Timeout time.Duration
}

// serviceName es el formato de un servicio de Compose: empieza con letra o
// número, así nunca se lee como una opción.
var serviceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// NameFromFile devuelve el nombre de proyecto que corresponde a un archivo, o
// ErrNotProjectFile si no es un archivo de proyecto.
func NameFromFile(filename string) (string, error) {
	base := path.Base(filename)
	for _, ext := range []string{".yaml", ".yml"} {
		if name, ok := strings.CutSuffix(base, ext); ok {
			return name, nil
		}
	}
	return "", ErrNotProjectFile
}

// Parse lee un project.yaml y devuelve el proyecto validado. filename es el
// nombre del archivo, del que sale el nombre que tiene que declarar.
func Parse(filename string, data []byte) (proto.Project, error) {
	spec, err := ParseSpec(filename, data)
	if err != nil {
		return proto.Project{}, err
	}
	return spec.Project, nil
}

// ParseSpec lee un project.yaml como Parse y devuelve además los pasos
// declarados del deploy, validados.
func ParseSpec(filename string, data []byte) (Spec, error) {
	expected, err := NameFromFile(filename)
	if err != nil {
		return Spec{}, err
	}
	if len(data) > MaxFileBytes {
		return Spec{}, fmt.Errorf("el archivo pesa %d bytes y el máximo es %d", len(data), MaxFileBytes)
	}

	var f file
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return Spec{}, errors.New("el archivo está vacío")
		}
		return Spec{}, yamlError(err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Spec{}, errors.New("el archivo tiene más de un documento YAML")
	}

	switch {
	case f.Name == "":
		return Spec{}, errors.New("name: falta")
	case f.Name != expected:
		return Spec{}, fmt.Errorf("name: tiene que ser igual al nombre del archivo (%s)", expected)
	}
	if f.Rollback != "" && (f.Rollback != RollbackPreviousImage || f.Runtime != string(proto.RuntimeDocker)) {
		return Spec{}, fmt.Errorf("rollback: el único valor aceptado es %s, y solo con runtime docker", RollbackPreviousImage)
	}

	project := proto.Project{
		Name:           f.Name,
		Environment:    proto.Environment(f.Environment),
		Repo:           f.Repo,
		Branch:         f.Branch,
		Runtime:        proto.ProjectRuntime(f.Runtime),
		Workdir:        f.Workdir,
		ComposeFile:    f.ComposeFile,
		ComposeProject: f.ComposeProject,
		Units:          f.Units,
	}
	if project.Runtime == proto.RuntimeDocker && project.ComposeProject == "" {
		project.ComposeProject = DefaultComposeProject(project.Workdir)
	}
	if f.Healthcheck != nil {
		spec := proto.HealthcheckSpec{
			URL:            f.Healthcheck.URL,
			ExpectStatus:   f.Healthcheck.ExpectStatus,
			TimeoutSeconds: f.Healthcheck.TimeoutSeconds,
		}
		if spec.ExpectStatus == 0 {
			spec.ExpectStatus = DefaultExpectStatus
		}
		if spec.TimeoutSeconds == 0 {
			spec.TimeoutSeconds = DefaultTimeoutSeconds
		}
		project.Healthcheck = &spec
	}

	if err := project.Validate(); err != nil {
		return Spec{}, err
	}

	spec := Spec{Project: project}
	if f.Deploy != nil {
		if (len(f.Deploy.Pre) > 0 || len(f.Deploy.Post) > 0) && project.Runtime != proto.RuntimeDocker {
			return Spec{}, errors.New("deploy.pre y deploy.post: por ahora son solo para runtime docker (los de systemd llegan en el Sprint 5)")
		}
		var err error
		if spec.Pre, err = parseSteps("deploy.pre", f.Deploy.Pre); err != nil {
			return Spec{}, err
		}
		if spec.Post, err = parseSteps("deploy.post", f.Deploy.Post); err != nil {
			return Spec{}, err
		}
	}
	return spec, nil
}

// parseSteps valida los pasos declarados de una lista (ADR-016). Como el resto
// de los errores, nombran el campo y la regla, nunca el valor.
func parseSteps(field string, raw []step) ([]Step, error) {
	if len(raw) > MaxSteps {
		return nil, fmt.Errorf("%s: trae %d pasos y el máximo es %d", field, len(raw), MaxSteps)
	}
	var steps []Step
	for i, s := range raw {
		where := fmt.Sprintf("%s[%d]", field, i)
		switch {
		case s.Service == "":
			return nil, fmt.Errorf("%s.service: falta", where)
		case len(s.Service) > maxStepServiceChars || !serviceName.MatchString(s.Service):
			return nil, fmt.Errorf("%s.service: tiene que ser un nombre de servicio de Compose (letras, números, _ . -)", where)
		case len(s.Command) == 0:
			return nil, fmt.Errorf("%s.command: falta; es una lista de argumentos, como [\"python\", \"manage.py\", \"migrate\"]", where)
		case len(s.Command) > MaxStepArgs:
			return nil, fmt.Errorf("%s.command: trae %d argumentos y el máximo es %d", where, len(s.Command), MaxStepArgs)
		case strings.HasPrefix(s.Command[0], "-"):
			return nil, fmt.Errorf("%s.command: el primer argumento no puede empezar con -", where)
		case s.TimeoutSeconds < 0 || s.TimeoutSeconds > maxStepTimeoutSecs:
			return nil, fmt.Errorf("%s.timeout_seconds: tiene que estar entre 1 y %d", where, maxStepTimeoutSecs)
		}
		for j, arg := range s.Command {
			if arg == "" || len(arg) > MaxStepArgLen || !printable(arg) {
				return nil, fmt.Errorf("%s.command[%d]: vacío, de más de %d bytes o con caracteres de control", where, j, MaxStepArgLen)
			}
		}
		timeout := DefaultStepTimeout
		if s.TimeoutSeconds > 0 {
			timeout = time.Duration(s.TimeoutSeconds) * time.Second
		}
		steps = append(steps, Step{Service: s.Service, Command: s.Command, Timeout: timeout})
	}
	return steps, nil
}

// printable acepta texto UTF-8 válido sin caracteres de control: un argumento
// con un salto de línea o una secuencia de escape es un error, no algo que se
// pasa tal cual.
func printable(s string) bool {
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

// composeUnsafe es lo que Docker Compose saca del nombre del directorio para
// armar el nombre del proyecto cuando nadie lo fija.
var composeUnsafe = regexp.MustCompile(`[^a-z0-9_-]`)

// DefaultComposeProject es el nombre de proyecto que usa Docker Compose cuando
// ni el archivo ni la línea de comando lo fijan: el directorio de trabajo, en
// minúsculas y sin caracteres fuera de [a-z0-9_-].
func DefaultComposeProject(workdir string) string {
	name := composeUnsafe.ReplaceAllString(strings.ToLower(path.Base(workdir)), "")
	return strings.TrimLeft(name, "_-")
}

// backticked son los valores que el paquete yaml cita en sus errores, y
// unknownField el error de un campo que no existe, que nombra el tipo de Go.
var (
	backticked   = regexp.MustCompile("`[^`]*`")
	unknownField = regexp.MustCompile(`field (\S+) not found in type \S+`)
)

// yamlError deja el error del paquete yaml sin los valores del archivo, que
// cita entre comillas invertidas ("cannot unmarshal !!str `abc` into int"): la
// línea y el tipo alcanzan para encontrarlo, y el contenido no viaja.
func yamlError(err error) error {
	msg := backticked.ReplaceAllString(err.Error(), "(valor)")
	msg = unknownField.ReplaceAllString(msg, "el campo $1 no existe")
	// Los pasos se leen en un tipo de Go; el error lo nombra por cómo se escribe.
	msg = strings.NewReplacer("[]projectspec.step", "una lista de pasos", "projectspec.step", "un paso {service, command, timeout_seconds}").Replace(msg)
	msg = strings.TrimPrefix(msg, "yaml: ")
	if len(msg) > proto.MaxProjectErrorLen-16 {
		msg = strings.ToValidUTF8(msg[:proto.MaxProjectErrorLen-16], "")
	}
	return fmt.Errorf("YAML inválido: %s", strings.ReplaceAll(msg, "\n", "; "))
}
