// Package accept es `statussv-agent project accept`: una persona, como root
// en el servidor, acepta la propuesta de un proyecto nuevo y lo declara
// (ADR-022).
//
// Es la segunda llave del alta. El control plane propone y el agent guarda la
// propuesta, pero los dos pueden estar comprometidos: para este comando la
// propuesta es texto no confiable. La vuelve a validar con el parser estricto
// y con lo que una propuesta no elige, muestra todo lo que va a hacer, pide
// que se escriba el nombre del proyecto para seguir, y escribe exactamente la
// declaración que mostró.
//
// Cada paso mira si ya está hecho, así correrlo de nuevo sigue donde quedó.
package accept

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/deploy"
	"github.com/JuanBaez77/status-sv/agent/internal/dropin"
	"github.com/JuanBaez77/status-sv/agent/internal/projects"
	"github.com/JuanBaez77/status-sv/agent/internal/proposals"
	"github.com/JuanBaez77/status-sv/internal/projectspec"
	"github.com/JuanBaez77/status-sv/internal/templates"
	"github.com/JuanBaez77/status-sv/proto"
)

// AgentUser es el usuario y el grupo del agent.
const AgentUser = projectspec.AgentUser

// Paths son las rutas del servidor que usa el comando.
type Paths struct {
	// State es el directorio de estado del agent; las propuestas están en
	// State/proposals y los diarios de deploy en State/deploys.
	State string
	// Projects es donde se declaran los proyectos.
	Projects string
	// Keys es donde viven las deploy keys, una por proyecto.
	Keys string
	// Dropin es el 20-deploy.conf de la unidad del agent.
	Dropin string
	// HelperSocket es la unidad del socket del ayudante root (ADR-019): un
	// proyecto systemd no se despliega sin él.
	HelperSocket string
	// Root se antepone al workdir del proyecto al tocar el disco. En el
	// servidor es vacía; en las pruebas, un directorio temporal.
	Root string
}

// DefaultHelperSocket es dónde instala el instalador el socket del ayudante.
const DefaultHelperSocket = "/etc/systemd/system/statussv-unit-helper.socket"

// System es lo que el comando le pide al sistema operativo. La de verdad usa
// useradd, ssh-keygen, systemctl y runuser; las pruebas, una de mentira.
type System interface {
	IsRoot() bool
	UserExists(name string) (bool, error)
	// CreateUser crea un usuario del sistema, sin shell, con su grupo y con
	// home en el directorio que se le dé, sin crearlo.
	CreateUser(name, home string) error
	Chown(path, user, group string) error
	// Owner devuelve los números de un usuario y un grupo, para cambiar el
	// dueño de un archivo abierto sin volver a buscarlo por su ruta.
	Owner(user, group string) (uid, gid int, err error)
	// Keygen genera una llave ed25519 sin contraseña en path (y path.pub).
	Keygen(path, comment string) error
	Hostname() string
	// RestartAgent recarga systemd y reinicia el agent.
	RestartAgent(ctx context.Context) error
	// DeployCheck corre deploy-check del proyecto como el usuario del agent,
	// mostrando lo que dice.
	DeployCheck(ctx context.Context, project string) error

	// Lo que sigue es para la unidad y el sitio (ADR-023).

	// DaemonReload le pide a systemd que relea las unidades.
	DaemonReload(ctx context.Context) error
	// VerifyUnit devuelve lo que dice systemd-analyze verify de una unidad.
	VerifyUnit(ctx context.Context, path string) (string, error)
	// Has dice si un programa está instalado.
	Has(program string) bool
	// Resolves dice si un dominio resuelve.
	Resolves(domain string) bool
	// NginxTest corre nginx -t, mostrando lo que dice si falla.
	NginxTest(ctx context.Context) error
	// NginxReload recarga nginx.
	NginxReload(ctx context.Context) error
	// Certbot corre certbot con esos argumentos.
	Certbot(ctx context.Context, args []string) error
	// Fetch pide una ruta de un dominio al nginx de este servidor, por HTTP,
	// y devuelve el cuerpo si contesta 200.
	Fetch(ctx context.Context, domain, path string) (string, error)
}

// Options son las banderas del comando.
type Options struct {
	// Yes acepta sin preguntar. Es para la prueba de punta a punta del CI.
	Yes bool
	// NoDeployKey no genera la deploy key: el repo es público y se trae por
	// HTTPS.
	NoDeployKey bool
	// EnvFile es un archivo con los valores de las variables, en vez de
	// pedirlos por la terminal. Es para la prueba de punta a punta del CI.
	EnvFile string
	// Email es el correo para la cuenta de Let's Encrypt, si el servidor
	// todavía no tiene una.
	Email string
}

// Accepter acepta propuestas.
type Accepter struct {
	Paths Paths
	Sys   System
	In    io.Reader
	Out   io.Writer
	Now   func() time.Time
	// ReadSecret pide un valor sin mostrarlo. Si es nil, se lee de la
	// terminal sin eco o, si In no es una terminal, un renglón de In.
	ReadSecret func(prompt string) (string, error)
	// LoadSpec lee la declaración de un proyecto ya declarado (project env).
	// En el servidor es la del ayudante, que exige que el archivo sea de root.
	LoadSpec func(dir, name string) (projectspec.Spec, error)
	// Templates devuelve las plantillas de la unidad y del sitio. Si es nil,
	// las de fábrica.
	Templates func() (*templates.Set, error)

	// in lee de In: uno solo para todas las preguntas, así no se pierde
	// lo que ya leyó otro.
	in *bufio.Reader
}

// ErrNotRoot es que el comando no corre como root.
var ErrNotRoot = errors.New("este comando crea usuarios, directorios y archivos de root: correlo con sudo")

// ErrAborted es que la persona no confirmó.
var ErrAborted = errors.New("no se aceptó la propuesta: no se cambió nada")

// action es un paso del alta. done dice que ya estaba hecho: se muestra pero
// no se repite.
type action struct {
	what  string
	done  bool
	apply func(ctx context.Context) error
}

// acceptance es lo que se sabe de una propuesta mientras se acepta.
type acceptance struct {
	name     string
	yaml     []byte
	spec     projectspec.Spec
	opts     Options
	declared bool
	// keyCreated es que la deploy key se generó en esta corrida: hay que
	// cargarla en GitHub antes de deploy-check.
	keyCreated bool
	// set son las plantillas; unit y site, lo que generan para el proyecto.
	set  *templates.Set
	unit []byte
	site []byte
}

// Accept acepta la propuesta del proyecto name.
func (a *Accepter) Accept(ctx context.Context, name string, opts Options) error {
	if !a.Sys.IsRoot() {
		return ErrNotRoot
	}
	if _, err := os.Stat(a.Paths.Dropin); err != nil {
		return errors.New("el deploy está apagado en este servidor: prendelo antes con install-agent.sh --enable-deploy")
	}
	acc, err := a.load(name, opts)
	if err != nil {
		return err
	}
	if err := a.checkNoDeploys(); err != nil {
		return err
	}

	plan, err := a.plan(acc)
	if err != nil {
		return err
	}
	a.show(acc, plan)
	if !opts.Yes {
		if err := a.confirm(name); err != nil {
			return err
		}
	}
	for _, step := range plan {
		if step.done {
			continue
		}
		a.printf("\n==> %s\n", step.what)
		if err := step.apply(ctx); err != nil {
			return fmt.Errorf("%s: %w", step.what, err)
		}
	}
	a.printf("\nListo: %s quedó declarado. El primer deploy se pide desde el panel.\n", name)
	return nil
}

// load lee la propuesta guardada y la valida como si nunca se hubiera visto.
func (a *Accepter) load(name string, opts Options) (*acceptance, error) {
	if !proto.ValidProjectName(name) {
		return nil, errors.New("nombre de proyecto inválido")
	}
	p, err := proposals.Load(filepath.Join(a.Paths.State, proposals.DirName), name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no hay una propuesta de %s en este servidor: mirá en el panel si el agent la recibió", name)
	}
	if err != nil {
		return nil, fmt.Errorf("no se pudo leer la propuesta: %w", err)
	}
	if !p.ExpiresAt.After(a.now()) {
		return nil, fmt.Errorf("la propuesta venció el %s: hay que proponerla de nuevo", p.ExpiresAt.Local().Format("2006-01-02 15:04"))
	}
	spec, err := projectspec.ParseSpec(name+".yaml", p.YAML)
	if err != nil {
		return nil, fmt.Errorf("la propuesta no es un project.yaml válido: %w", err)
	}
	if err := projectspec.CheckProposal(spec); err != nil {
		return nil, fmt.Errorf("la propuesta elige algo que no puede elegir: %w", err)
	}
	acc := &acceptance{name: name, yaml: p.YAML, spec: spec, opts: opts}

	// Si ya está declarado con lo mismo, es una corrida anterior que quedó
	// por la mitad: se sigue. Con otra cosa, no se pisa.
	for _, ext := range []string{".yaml", ".yml"} {
		existing, err := os.ReadFile(filepath.Join(a.Paths.Projects, name+ext)) //nolint:gosec // nombre validado
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return nil, fmt.Errorf("no se pudo leer la declaración que ya existe: %w", err)
		case ext == ".yaml" && bytes.Equal(existing, p.YAML):
			acc.declared = true
		default:
			return nil, fmt.Errorf("ya hay un proyecto %s declarado en %s, distinto de la propuesta: no se pisa", name, a.Paths.Projects)
		}
	}
	return acc, nil
}

// checkNoDeploys se niega si hay un deploy en curso: aceptar reinicia el
// agent, y un reinicio en medio de un deploy lo vuelve atrás.
func (a *Accepter) checkNoDeploys() error {
	active, err := deploy.Active(a.Paths.State)
	if err != nil {
		return fmt.Errorf("no se pudo saber si hay un deploy en curso: %w", err)
	}
	if len(active) > 0 {
		return fmt.Errorf("hay un deploy en curso (%s): esperá a que termine, porque aceptar reinicia el agent", strings.Join(active, ", "))
	}
	return nil
}

// plan arma los pasos, mirando cuáles ya están hechos.
func (a *Accepter) plan(acc *acceptance) ([]action, error) {
	var plan []action
	project := acc.spec.Project
	systemd := project.Runtime == proto.RuntimeSystemd

	// Las plantillas se cargan y se llenan antes de tocar nada: una plantilla
	// del servidor que no anda frena el alta antes de empezar.
	set, err := a.templateSet()
	if err != nil {
		return nil, err
	}
	acc.set = set
	if acc.spec.Service != nil {
		if acc.unit, err = set.Service(acc.spec); err != nil {
			return nil, err
		}
	}
	if acc.spec.Site != nil {
		if acc.site, err = set.Site(acc.spec, templates.SiteTLS); err != nil {
			return nil, err
		}
	}

	if systemd {
		exists, err := a.Sys.UserExists(acc.spec.RunAs)
		if err != nil {
			return nil, err
		}
		step := action{what: fmt.Sprintf("crear el usuario del sistema %s, sin shell, con home en %s", acc.spec.RunAs, project.Workdir), done: exists}
		if exists {
			step.what = fmt.Sprintf("el usuario %s ya existe: se usa", acc.spec.RunAs)
		}
		step.apply = func(context.Context) error { return a.Sys.CreateUser(acc.spec.RunAs, project.Workdir) }
		plan = append(plan, step)
	}

	owner := "root"
	if systemd {
		owner = acc.spec.RunAs
	}
	workdirWhat := fmt.Sprintf("crear %s, de %s y del grupo %s (2775)", project.Workdir, owner, AgentUser)
	if _, err := os.Stat(a.onDisk(project.Workdir)); err == nil {
		workdirWhat = fmt.Sprintf("%s ya existe: queda de %s y del grupo %s (2775)", project.Workdir, owner, AgentUser)
	}
	plan = append(plan, action{what: workdirWhat, apply: func(context.Context) error {
		return a.makeWorkdir(a.onDisk(project.Workdir), owner)
	}})

	if !acc.opts.NoDeployKey {
		key := filepath.Join(a.Paths.Keys, acc.name)
		_, err := os.Stat(key)
		exists := err == nil
		step := action{what: fmt.Sprintf("generar la deploy key ed25519 en %s", key), done: exists}
		if exists {
			step.what = fmt.Sprintf("la deploy key %s ya existe: se usa", key)
		}
		step.apply = func(context.Context) error {
			if err := a.makeKey(key, acc.name); err != nil {
				return err
			}
			acc.keyCreated = true
			return nil
		}
		plan = append(plan, step)
	}

	if step, ok := a.secretsStep(acc); ok {
		plan = append(plan, step)
	}
	if step, ok, err := a.unitStep(acc); err != nil {
		return nil, err
	} else if ok {
		plan = append(plan, step)
	}

	declare := action{
		what: fmt.Sprintf("declarar el proyecto en %s", filepath.Join(a.Paths.Projects, acc.name+".yaml")),
		done: acc.declared,
		apply: func(context.Context) error {
			return a.writeRootFile(filepath.Join(a.Paths.Projects, acc.name+".yaml"), acc.yaml, 0o640, AgentUser)
		},
	}
	if acc.declared {
		declare.what = "el proyecto ya está declarado igual a la propuesta"
	}
	plan = append(plan, declare)
	if step, ok := a.siteStep(acc); ok {
		plan = append(plan, step)
	}

	plan = append(plan, action{
		what:  fmt.Sprintf("sumar %s al drop-in del deploy y reiniciar el agent", project.Workdir),
		apply: func(ctx context.Context) error { return a.restart(ctx) },
	})
	plan = append(plan, action{
		what:  "revisar el proyecto con deploy-check",
		apply: func(ctx context.Context) error { return a.check(ctx, acc) },
	})
	// Al final, no antes: si algo falla en el medio, correr el comando de
	// nuevo necesita la propuesta.
	plan = append(plan, action{
		what: "borrar la propuesta, que ya se aceptó",
		apply: func(context.Context) error {
			return proposals.Remove(filepath.Join(a.Paths.State, proposals.DirName), acc.name)
		},
	})
	return plan, nil
}

// show muestra la declaración entera y los pasos.
func (a *Accepter) show(acc *acceptance, plan []action) {
	a.printf("Propuesta de %s (%s, %s)\n\n", acc.name, acc.spec.Project.Runtime, acc.spec.Project.Environment)
	a.printf("--- %s.yaml: esto es lo que se va a declarar ---\n", acc.name)
	a.printf("%s", acc.yaml)
	if !bytes.HasSuffix(acc.yaml, []byte("\n")) {
		a.printf("\n")
	}
	a.printf("---\n\n")
	if steps := allSteps(acc.spec); len(steps) > 0 {
		a.printf("Estos comandos van a correr en este servidor en cada deploy, con el usuario del proyecto:\n")
		for _, s := range steps {
			a.printf("  %s\n", s)
		}
		a.printf("\n")
	}
	if acc.unit != nil {
		a.printf("--- %s: la unidad, con la plantilla %s ---\n%s---\n\n", templates.ServiceUnitFile(acc.name), acc.set.ServiceOrigin, acc.unit)
	}
	if acc.site != nil {
		a.printf("--- %s: el sitio, con la plantilla %s ---\n%s---\n\n", templates.SiteFileName(acc.name), acc.set.SiteOrigin, acc.site)
	}
	a.printf("Pasos:\n")
	for i, step := range plan {
		a.printf("  %d. %s\n", i+1, step.what)
	}
}

// allSteps son los comandos declarados, para mostrarlos aparte: son lo que va
// a correr en el servidor.
func allSteps(spec projectspec.Spec) []string {
	var out []string
	add := func(phase string, steps []projectspec.Step) {
		for _, s := range steps {
			cmd := strings.Join(s.Command, " ")
			if s.Service != "" {
				cmd = s.Service + ": " + cmd
			}
			out = append(out, phase+": "+cmd)
		}
	}
	add("build", spec.Build)
	add("pre", spec.Pre)
	add("post", spec.Post)
	if spec.Service != nil {
		out = append(out, "servicio: "+strings.Join(spec.Service.Command, " "))
	}
	return out
}

// confirm pide que se escriba el nombre del proyecto.
func (a *Accepter) confirm(name string) error {
	a.printf("\nPara aceptar, escribí el nombre del proyecto (%s): ", name)
	line, err := a.reader().ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.TrimSpace(line) != name {
		return ErrAborted
	}
	return nil
}

// makeWorkdir crea el workdir, o le ajusta el dueño y los permisos si ya
// existe: del dueño que corresponde, del grupo del agent, 2775 (el grupo
// escribe y lo nuevo hereda el grupo).
func (a *Accepter) makeWorkdir(dir, owner string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // los permisos finales los fija el Chmod de abajo
		return err
	}
	if err := a.Sys.Chown(dir, owner, AgentUser); err != nil {
		return err
	}
	return os.Chmod(dir, os.ModeSetgid|0o775) //nolint:gosec // el grupo del agent escribe ahí (runbook, primer deploy)
}

// makeKey genera la deploy key: la privada del agent y 0400, como en el
// runbook (ssh rechaza una llave que otros pueden leer), y la pública 0644,
// que el agent informa para mostrarla en el panel.
func (a *Accepter) makeKey(key, name string) error {
	if err := a.Sys.Keygen(key, fmt.Sprintf("status-sv %s@%s", name, a.Sys.Hostname())); err != nil {
		return err
	}
	if err := a.Sys.Chown(key, AgentUser, AgentUser); err != nil {
		return err
	}
	if err := os.Chmod(key, 0o400); err != nil {
		return err
	}
	return os.Chmod(key+".pub", 0o644) //nolint:gosec // es la llave pública
}

// writeRootFile escribe un archivo de root entero o nada. El dueño y los
// permisos se cambian sobre el archivo abierto, no por su ruta: el .env de un
// proyecto Docker vive en un directorio donde el grupo del agent escribe, y
// entre escribir y cambiar los permisos podría reemplazar la ruta por un
// enlace a otro archivo.
func (a *Accepter) writeRootFile(path string, data []byte, mode os.FileMode, group string) error {
	uid, gid, err := a.Sys.Owner("root", group)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".statussv-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := fchown(tmp, uid, gid); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// restart reescribe el drop-in con el workdir nuevo, si hace falta, y
// reinicia el agent para que pueda escribir en él. Reinicia siempre: si una
// corrida anterior escribió el drop-in y el reinicio falló, el agent todavía
// no lo tomó.
func (a *Accepter) restart(ctx context.Context) error {
	state := projects.Read(a.Paths.Projects, a.now())
	if state.Source.Status != proto.SourceOK {
		return fmt.Errorf("no se pudieron leer los proyectos declarados: %s", state.Source.Detail)
	}
	content, warnings := dropin.Render(state)
	for _, w := range warnings {
		a.printf("aviso: %s\n", w)
	}
	// Otra vez, justo antes: pudo empezar un deploy mientras se confirmaba.
	if err := a.checkNoDeploys(); err != nil {
		return err
	}
	if current, err := os.ReadFile(a.Paths.Dropin); err != nil || string(current) != content {
		if err := a.writeRootFile(a.Paths.Dropin, []byte(content), 0o644, "root"); err != nil {
			return err
		}
	}
	return a.Sys.RestartAgent(ctx)
}

// check muestra la llave para cargar en GitHub, si se generó ahora, y corre
// deploy-check. Que falle no es un error del alta: dice qué falta.
func (a *Accepter) check(ctx context.Context, acc *acceptance) error {
	if acc.spec.Project.Runtime == proto.RuntimeSystemd {
		if _, err := os.Stat(a.Paths.HelperSocket); err != nil {
			a.printf("aviso: el ayudante root de systemd no está instalado (ADR-019): corré install-agent.sh --enable-deploy de nuevo para instalarlo\n")
		}
	}
	if acc.keyCreated {
		a.showKey(acc)
		if !acc.opts.Yes {
			a.printf("\nCuando la cargues, apretá Enter para seguir con deploy-check: ")
			_, _ = a.reader().ReadString('\n')
		}
	}
	if err := a.Sys.DeployCheck(ctx, acc.name); err != nil {
		a.printf("\naviso: deploy-check encontró problemas (arriba dice cuáles); el proyecto quedó declarado, y se puede revisar de nuevo con:\n")
		a.printf("  sudo runuser -u %s -- statussv-agent deploy-check %s\n", AgentUser, acc.name)
	}
	return nil
}

// showKey muestra la llave pública y cómo cargarla en GitHub, de solo lectura.
func (a *Accepter) showKey(acc *acceptance) {
	pub, err := os.ReadFile(filepath.Join(a.Paths.Keys, acc.name+".pub")) //nolint:gosec // nombre validado
	if err != nil {
		a.printf("aviso: no se pudo leer la llave pública: %v\n", err)
		return
	}
	key := strings.TrimSpace(string(pub))
	a.printf("\nLa deploy key, para cargar en GitHub como deploy key de SOLO LECTURA:\n\n  %s\n\n", key)
	if repo, ok := githubRepo(acc.spec.Project.Repo); ok {
		a.printf("Desde tu PC, con gh:\n\n")
		a.printf("  echo \"%s\" > statussv-%s.pub\n", key, acc.name)
		a.printf("  gh repo deploy-key add statussv-%s.pub --repo %s --title \"status-sv %s\"\n", acc.name, repo, a.Sys.Hostname())
	} else {
		a.printf("En la configuración del repo, en Deploy keys, sin permiso de escritura.\n")
	}
}

// githubRepo saca dueño/repo de un repo de GitHub declarado.
func githubRepo(repo string) (string, bool) {
	repo = strings.TrimSuffix(repo, ".git")
	for _, prefix := range []string{"https://github.com/", "ssh://git@github.com/", "git@github.com:", "github.com/"} {
		if rest, ok := strings.CutPrefix(repo, prefix); ok && strings.Count(rest, "/") == 1 {
			return rest, true
		}
	}
	return "", false
}

func (a *Accepter) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(a.Out, format, args...)
}

func (a *Accepter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Accepter) reader() *bufio.Reader {
	if a.in == nil {
		a.in = bufio.NewReader(a.In)
	}
	return a.in
}

// onDisk es la ruta de un directorio del proyecto en el disco.
func (a *Accepter) onDisk(path string) string {
	if a.Paths.Root == "" {
		return path
	}
	return filepath.Join(a.Paths.Root, filepath.FromSlash(path))
}
