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
// campos desconocidos, que el nombre coincida con el del archivo y lo que solo
// usa quien despliega y no viaja al control plane: los pasos declarados del
// deploy (ADR-016) y, en un proyecto systemd, con qué usuario y con qué archivo
// de entorno corren (ADR-019).
//
// Lo usan el agent, el ayudante root y, para validar una propuesta de un
// proyecto nuevo, el control plane (ADR-022): las mismas reglas en los tres.
package projectspec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
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

// Los rollbacks declarables, uno por runtime: con Docker, volver a la imagen
// anterior (ADR-015); con systemd, al commit anterior, corriendo deploy.build
// de nuevo (ADR-019). Declararlo es opcional: cada runtime tiene uno solo.
const (
	RollbackPreviousImage  = "previous_image"
	RollbackPreviousCommit = "previous_commit"
)

// AgentUser es el usuario del agent. Los pasos de un proyecto no pueden correr
// con él: tendrían a mano su credencial y las deploy keys de todos.
const AgentUser = "statussv"

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
	RunAs          string       `yaml:"run_as"`
	EnvFile        string       `yaml:"env_file"`
	Healthcheck    *healthcheck `yaml:"healthcheck"`
	Deploy         *deploySteps `yaml:"deploy"`
	Rollback       string       `yaml:"rollback"`
	RequiredEnv    []string     `yaml:"required_env"`
	Service        *service     `yaml:"service"`
	Site           *site        `yaml:"site"`
}

// service es el servicio de un proyecto systemd cuya unidad se genera desde la
// plantilla (ADR-023).
type service struct {
	Command []string `yaml:"command"`
}

// site es el sitio de nginx de un proyecto (ADR-023).
type site struct {
	Domain string `yaml:"domain"`
	Port   int    `yaml:"port"`
}

type healthcheck struct {
	URL            string `yaml:"url"`
	ExpectStatus   int    `yaml:"expect_status"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

// deploySteps son los pasos declarados del deploy (ADR-016). Build es solo de
// los proyectos systemd (ADR-019).
type deploySteps struct {
	Build []step `yaml:"build"`
	Pre   []step `yaml:"pre"`
	Post  []step `yaml:"post"`
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
	maxEnvFileLen       = 256
)

// Lo que una propuesta no elige (ADR-022): el workdir y el archivo de entorno
// salen del nombre del proyecto.
const (
	ProposalBaseDir = "/srv"
	ProposalEnvDir  = "/etc/status-sv/env"
)

// Spec es un proyecto declarado con lo que solo usa quien despliega. Project
// es lo que viaja al control plane; lo demás no, porque un comando o una ruta
// podrían traer algo que no tiene por qué salir del servidor.
type Spec struct {
	Project proto.Project
	// Build son los pasos que preparan el código de un proyecto systemd
	// (dependencias, estáticos). Se repiten en un rollback.
	Build []Step
	Pre   []Step
	Post  []Step
	// RunAs es el usuario con el que corren los pasos de un proyecto systemd.
	RunAs string
	// EnvFile es el archivo de entorno de esos pasos, si se declaró. Lo lee
	// systemd; el agent no lo abre.
	EnvFile string
	// RequiredEnv son los nombres de las variables de entorno que el proyecto
	// necesita para arrancar (ADR-022). Solo los nombres: los valores se
	// cargan en el servidor y no salen de él.
	RequiredEnv []string
	// Service es el servicio de un proyecto systemd cuya unidad se genera
	// desde la plantilla (ADR-023). Con él, Units es [<nombre>.service].
	Service *Service
	// Site es el sitio de nginx del proyecto, si tiene uno (ADR-023).
	Site *Site
}

// Service es el servicio que corre un proyecto systemd con la unidad generada.
type Service struct {
	// Command es el comando tal como se escribió, para mostrarlo.
	Command []string
	// Exec es Command[0] resuelto a una ruta absoluta, como en un paso.
	Exec string
}

// Site es el sitio de nginx de un proyecto: su dominio y el puerto de
// 127.0.0.1 al que pasa los pedidos.
type Site struct {
	Domain string
	Port   int
}

// Step es un paso declarado. En un proyecto Docker corre con docker compose
// run en Service, con la imagen recién compilada. En uno systemd no lleva
// servicio: corre en el servidor, con el usuario del proyecto.
type Step struct {
	Service string
	Command []string
	Timeout time.Duration
	// Exec es Command[0] resuelto a una ruta absoluta. Solo en proyectos
	// systemd: ahí el programa no se busca en el PATH.
	Exec string
}

// serviceName es el formato de un servicio de Compose: empieza con letra o
// número, así nunca se lee como una opción.
var serviceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// userName es el formato de un usuario del sistema, el que acepta useradd.
var userName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

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
	if err := checkRollback(f.Rollback, f.Runtime); err != nil {
		return Spec{}, err
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
	// Con la unidad generada, el proyecto tiene esa unidad y ninguna otra:
	// declararla es opcional.
	if project.Runtime == proto.RuntimeSystemd && f.Service != nil && len(project.Units) == 0 {
		project.Units = []string{ServiceUnit(f.Name)}
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
	systemd := project.Runtime == proto.RuntimeSystemd
	if systemd {
		// Sin pasos ni unidad generada no hace falta un usuario: desplegar es
		// traer el código y reiniciar. Así un proyecto que ya estaba declarado
		// sigue valiendo.
		steps := f.Deploy != nil && len(f.Deploy.Build)+len(f.Deploy.Pre)+len(f.Deploy.Post) > 0
		if err := checkRun(f.RunAs, f.EnvFile, steps || f.Service != nil); err != nil {
			return Spec{}, err
		}
		spec.RunAs, spec.EnvFile = f.RunAs, f.EnvFile
	} else if f.RunAs != "" || f.EnvFile != "" {
		return Spec{}, errors.New("run_as y env_file: son solo para proyectos de systemd")
	}
	if err := checkRequiredEnv(f.RequiredEnv); err != nil {
		return Spec{}, err
	}
	if systemd && len(f.RequiredEnv) > 0 && f.EnvFile == "" {
		return Spec{}, errors.New("required_env: con systemd necesita env_file, el archivo donde quedan los valores")
	}
	spec.RequiredEnv = f.RequiredEnv
	if f.Service != nil {
		if spec.Service, err = parseService(f.Service, project); err != nil {
			return Spec{}, err
		}
	}
	if f.Site != nil {
		if err := checkSite(f.Site); err != nil {
			return Spec{}, err
		}
		spec.Site = &Site{Domain: f.Site.Domain, Port: f.Site.Port}
		// El sitio es público: viaja al control plane con el proyecto.
		spec.Project.Site = &proto.SiteSpec{Domain: f.Site.Domain, Port: f.Site.Port}
	}
	if f.Deploy != nil {
		if len(f.Deploy.Build) > 0 && !systemd {
			return Spec{}, errors.New("deploy.build: es solo para proyectos de systemd; con Docker compila docker compose build")
		}
		var err error
		if spec.Build, err = parseSteps("deploy.build", f.Deploy.Build, project); err != nil {
			return Spec{}, err
		}
		if spec.Pre, err = parseSteps("deploy.pre", f.Deploy.Pre, project); err != nil {
			return Spec{}, err
		}
		if spec.Post, err = parseSteps("deploy.post", f.Deploy.Post, project); err != nil {
			return Spec{}, err
		}
	}
	return spec, nil
}

// checkRollback revisa el rollback declarado contra el runtime: cada uno tiene
// el suyo, y no declararlo es lo mismo que declarar ese.
func checkRollback(rollback, runtime string) error {
	switch {
	case rollback == "":
		return nil
	case runtime == string(proto.RuntimeDocker) && rollback != RollbackPreviousImage:
		return fmt.Errorf("rollback: con runtime docker el único valor aceptado es %s", RollbackPreviousImage)
	case runtime == string(proto.RuntimeSystemd) && rollback != RollbackPreviousCommit:
		return fmt.Errorf("rollback: con runtime systemd el único valor aceptado es %s", RollbackPreviousCommit)
	}
	return nil
}

// checkRun valida con qué usuario y con qué archivo de entorno corren los
// pasos de un proyecto systemd (ADR-019) y su servicio, si la unidad se genera
// (ADR-023). El usuario hace falta si hay alguno de los dos.
func checkRun(runAs, envFile string, needsUser bool) error {
	switch {
	case runAs == "" && needsUser:
		return errors.New("run_as: falta; es el usuario con el que corren los pasos y el servicio del proyecto")
	case runAs == "" && envFile != "":
		return errors.New("env_file: necesita run_as; es el entorno de los pasos del proyecto")
	case runAs == "":
		return nil
	case !userName.MatchString(runAs):
		return errors.New("run_as: tiene que ser un nombre de usuario del sistema (minúsculas, números, _ y -)")
	case runAs == "root" || runAs == AgentUser:
		return fmt.Errorf("run_as: no puede ser root ni %s; el proyecto necesita un usuario propio", AgentUser)
	}
	if envFile == "" {
		return nil
	}
	if len(envFile) > maxEnvFileLen || !printable(envFile) || !path.IsAbs(envFile) || path.Clean(envFile) != envFile || envFile == "/" {
		return errors.New("env_file: tiene que ser la ruta absoluta y limpia de un archivo")
	}
	return nil
}

// parseSteps valida los pasos declarados de una lista (ADR-016 y ADR-019).
// Como el resto de los errores, nombran el campo y la regla, nunca el valor.
//
// Con Docker, un paso corre en un servicio de Compose. Con systemd no hay
// servicio, y el primer argumento es la ruta del programa: absoluta, o
// relativa al workdir y sin salir de él. No se busca en el PATH.
func parseSteps(field string, raw []step, project proto.Project) ([]Step, error) {
	systemd := project.Runtime == proto.RuntimeSystemd
	if len(raw) > MaxSteps {
		return nil, fmt.Errorf("%s: trae %d pasos y el máximo es %d", field, len(raw), MaxSteps)
	}
	var steps []Step
	for i, s := range raw {
		where := fmt.Sprintf("%s[%d]", field, i)
		switch {
		case systemd && s.Service != "":
			return nil, fmt.Errorf("%s.service: es solo para proyectos de Docker; con systemd el paso corre en el servidor", where)
		case !systemd && s.Service == "":
			return nil, fmt.Errorf("%s.service: falta", where)
		case !systemd && (len(s.Service) > maxStepServiceChars || !serviceName.MatchString(s.Service)):
			return nil, fmt.Errorf("%s.service: tiene que ser un nombre de servicio de Compose (letras, números, _ . -)", where)
		case s.TimeoutSeconds < 0 || s.TimeoutSeconds > maxStepTimeoutSecs:
			return nil, fmt.Errorf("%s.timeout_seconds: tiene que estar entre 1 y %d", where, maxStepTimeoutSecs)
		}
		if err := checkCommand(where, s.Command); err != nil {
			return nil, err
		}
		timeout := DefaultStepTimeout
		if s.TimeoutSeconds > 0 {
			timeout = time.Duration(s.TimeoutSeconds) * time.Second
		}
		parsed := Step{Service: s.Service, Command: s.Command, Timeout: timeout}
		if systemd {
			if parsed.Exec = execPath(project.Workdir, s.Command[0]); parsed.Exec == "" {
				return nil, fmt.Errorf("%s.command[0]: tiene que ser la ruta del programa, absoluta o relativa al workdir y sin salir de él", where)
			}
		}
		steps = append(steps, parsed)
	}
	return steps, nil
}

// checkCommand valida un comando declarado como lista de argumentos: el de un
// paso o el del servicio. where es el campo, para el error.
func checkCommand(where string, command []string) error {
	switch {
	case len(command) == 0:
		return fmt.Errorf("%s.command: falta; es una lista de argumentos, como [\"python\", \"manage.py\", \"migrate\"]", where)
	case len(command) > MaxStepArgs:
		return fmt.Errorf("%s.command: trae %d argumentos y el máximo es %d", where, len(command), MaxStepArgs)
	case strings.HasPrefix(command[0], "-"):
		return fmt.Errorf("%s.command: el primer argumento no puede empezar con -", where)
	}
	for j, arg := range command {
		if arg == "" || len(arg) > MaxStepArgLen || !printable(arg) {
			return fmt.Errorf("%s.command[%d]: vacío, de más de %d bytes o con caracteres de control", where, j, MaxStepArgLen)
		}
	}
	return nil
}

// parseService valida el servicio de la unidad generada (ADR-023): solo con
// systemd, con la unidad que lleva el nombre del proyecto y con un programa
// que, como el de un paso, es una ruta absoluta o relativa al workdir.
func parseService(raw *service, project proto.Project) (*Service, error) {
	unit := ServiceUnit(project.Name)
	switch {
	case project.Runtime != proto.RuntimeSystemd:
		return nil, errors.New("service: es solo para proyectos de systemd; con Docker los servicios los levanta docker compose")
	case len(project.Units) != 1 || project.Units[0] != unit:
		return nil, errors.New("units: con service es solo la unidad que se genera, <nombre>.service")
	}
	if err := checkCommand("service", raw.Command); err != nil {
		return nil, err
	}
	exec := execPath(project.Workdir, raw.Command[0])
	if exec == "" {
		return nil, errors.New("service.command[0]: tiene que ser la ruta del programa, absoluta o relativa al workdir y sin salir de él")
	}
	return &Service{Command: raw.Command, Exec: exec}, nil
}

// ServiceUnit es el nombre de la unidad que se genera para un proyecto.
func ServiceUnit(project string) string {
	return project + ".service"
}

// checkRequiredEnv valida los nombres de required_env.
func checkRequiredEnv(names []string) error {
	if len(names) > proto.MaxRequiredEnv {
		return fmt.Errorf("required_env: trae %d nombres y el máximo es %d", len(names), proto.MaxRequiredEnv)
	}
	seen := make(map[string]bool, len(names))
	for i, name := range names {
		switch {
		case !proto.ValidEnvName(name):
			return fmt.Errorf("required_env[%d]: tiene que ser un nombre de variable (mayúsculas, números y _, de hasta %d caracteres)", i, proto.MaxEnvNameLen)
		case seen[name]:
			return fmt.Errorf("required_env[%d]: está repetido", i)
		}
		seen[name] = true
	}
	return nil
}

// checkSite valida el sitio de nginx (ADR-023), con las reglas del contrato:
// el sitio viaja al control plane.
func checkSite(s *site) error {
	if err := (proto.SiteSpec{Domain: s.Domain, Port: s.Port}).Validate(); err != nil {
		return fmt.Errorf("site.%w", err)
	}
	return nil
}

// ProposalWorkdir es el workdir de un proyecto propuesto, que no se elige.
func ProposalWorkdir(project string) string {
	return ProposalBaseDir + "/" + project
}

// ProposalEnvFile es el archivo de entorno de un proyecto systemd propuesto.
func ProposalEnvFile(project string) string {
	return ProposalEnvDir + "/" + project + ".env"
}

// CheckProposal revisa lo que una propuesta no elige (ADR-022): el workdir, y
// en un proyecto systemd el usuario, el archivo de entorno y la unidad, que
// salen del nombre del proyecto. Se aplica además de ParseSpec, sobre lo que
// ya pasó el parser: una propuesta es un project.yaml con menos libertad.
//
// Como los del parser, los errores nombran el campo y la regla, no el valor.
func CheckProposal(spec Spec) error {
	project := spec.Project
	if project.Workdir != ProposalWorkdir(project.Name) {
		return errors.New("workdir: en una propuesta es siempre /srv/<nombre>")
	}
	if err := checkProposalRepo(project.Repo); err != nil {
		return err
	}
	if project.Runtime != proto.RuntimeSystemd {
		return nil
	}
	switch {
	case !userName.MatchString(project.Name):
		return errors.New("name: un proyecto systemd propuesto corre con un usuario de su mismo nombre, así que empieza con letra y tiene hasta 32 caracteres")
	case spec.Service == nil:
		return errors.New("service: falta; un proyecto systemd propuesto genera su unidad desde la plantilla")
	case spec.RunAs != project.Name:
		return errors.New("run_as: en una propuesta es siempre el nombre del proyecto")
	case spec.EnvFile != ProposalEnvFile(project.Name):
		return errors.New("env_file: en una propuesta es siempre /etc/status-sv/env/<nombre>.env")
	}
	return nil
}

// checkProposalRepo rechaza un repo con credenciales: una propuesta pasa por
// el control plane y queda en su base, y un secreto no viaja (regla 3). Para
// un repo privado está la deploy key.
func checkProposalRepo(repo string) error {
	const msg = "repo: una propuesta no lleva usuario con contraseña, token ni parámetros; para un repo privado está la deploy key"
	scheme, _, hasScheme := strings.Cut(repo, "://")
	if !hasScheme {
		// Forma corta o forma scp (git@servidor:dueño/repo): el usuario no
		// puede llevar contraseña.
		if user, _, ok := strings.Cut(repo, "@"); ok && strings.Contains(user, ":") {
			return errors.New(msg)
		}
		return nil
	}
	u, err := url.Parse(repo)
	if err != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return errors.New(msg)
	}
	// Un repo file:// no lleva credenciales, y el agent solo lo acepta en
	// pruebas (STATUSSV_DEPLOY_ALLOW_FILE_REPOS): lo usa el e2e del alta.
	if scheme == "file" {
		if u.Host != "" || u.User != nil {
			return errors.New(msg)
		}
		return nil
	}
	if u.Host == "" {
		return errors.New(msg)
	}
	switch scheme {
	case "https":
		if u.User != nil {
			return errors.New(msg)
		}
	case "ssh":
		if _, hasPassword := u.User.Password(); hasPassword {
			return errors.New(msg)
		}
	default:
		return errors.New("repo: en una propuesta, solo https o ssh (y file, que el agent acepta solo en pruebas)")
	}
	return nil
}

// execPath resuelve el programa de un paso de systemd a una ruta absoluta: tal
// cual si ya lo es, o adentro del workdir si es relativa. Devuelve vacío si no
// es una ruta limpia o si se sale del workdir.
func execPath(workdir, program string) string {
	if path.IsAbs(program) {
		if path.Clean(program) != program || program == "/" {
			return ""
		}
		return program
	}
	joined := path.Join(workdir, program)
	if !strings.HasPrefix(joined, workdir+"/") {
		return ""
	}
	return joined
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
	msg = strings.NewReplacer(
		"[]projectspec.step", "una lista de pasos",
		"projectspec.step", "un paso {service, command, timeout_seconds}",
		"projectspec.service", "un servicio {command}",
		"projectspec.site", "un sitio {domain, port}",
	).Replace(msg)
	msg = strings.TrimPrefix(msg, "yaml: ")
	if len(msg) > proto.MaxProjectErrorLen-16 {
		msg = strings.ToValidUTF8(msg[:proto.MaxProjectErrorLen-16], "")
	}
	return fmt.Errorf("YAML inválido: %s", strings.ReplaceAll(msg, "\n", "; "))
}
