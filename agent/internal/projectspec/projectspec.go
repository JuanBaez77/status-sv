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
// campos desconocidos, que el nombre coincida con el del archivo y que todavía
// no haya pasos de deploy.
package projectspec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

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

// deploySteps son los pasos previos y posteriores del deploy. Su formato lo
// define el Sprint 4; hasta entonces tienen que venir vacíos (ADR-012). Se leen
// como nodos para poder rechazar cualquier contenido sin intentar entenderlo.
type deploySteps struct {
	Pre  yaml.Node `yaml:"pre"`
	Post yaml.Node `yaml:"post"`
}

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
	expected, err := NameFromFile(filename)
	if err != nil {
		return proto.Project{}, err
	}
	if len(data) > MaxFileBytes {
		return proto.Project{}, fmt.Errorf("el archivo pesa %d bytes y el máximo es %d", len(data), MaxFileBytes)
	}

	var f file
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return proto.Project{}, errors.New("el archivo está vacío")
		}
		return proto.Project{}, yamlError(err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return proto.Project{}, errors.New("el archivo tiene más de un documento YAML")
	}

	switch {
	case f.Name == "":
		return proto.Project{}, errors.New("name: falta")
	case f.Name != expected:
		return proto.Project{}, fmt.Errorf("name: tiene que ser igual al nombre del archivo (%s)", expected)
	}
	if f.Deploy != nil && (!emptyNode(f.Deploy.Pre) || !emptyNode(f.Deploy.Post)) {
		return proto.Project{}, errors.New("deploy.pre y deploy.post: tienen que estar vacíos hasta que se defina el formato de los pasos (Sprint 4)")
	}
	if f.Rollback != "" && (f.Rollback != RollbackPreviousImage || f.Runtime != string(proto.RuntimeDocker)) {
		return proto.Project{}, fmt.Errorf("rollback: el único valor aceptado es %s, y solo con runtime docker", RollbackPreviousImage)
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
		return proto.Project{}, err
	}
	return project, nil
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

// emptyNode dice si un nodo es nulo, falta o es una lista vacía.
func emptyNode(n yaml.Node) bool {
	switch n.Kind {
	case 0:
		return true
	case yaml.ScalarNode:
		return n.Tag == "!!null"
	case yaml.SequenceNode:
		return len(n.Content) == 0
	default:
		return false
	}
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
	msg = strings.TrimPrefix(msg, "yaml: ")
	if len(msg) > proto.MaxProjectErrorLen-16 {
		msg = strings.ToValidUTF8(msg[:proto.MaxProjectErrorLen-16], "")
	}
	return fmt.Errorf("YAML inválido: %s", strings.ReplaceAll(msg, "\n", "; "))
}
