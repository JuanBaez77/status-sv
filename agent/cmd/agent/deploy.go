package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/compose"
	"github.com/JuanBaez77/status-sv/agent/internal/config"
	"github.com/JuanBaez77/status-sv/agent/internal/docker"
	"github.com/JuanBaez77/status-sv/agent/internal/dropin"
	"github.com/JuanBaez77/status-sv/agent/internal/gitops"
	"github.com/JuanBaez77/status-sv/agent/internal/projects"
	"github.com/JuanBaez77/status-sv/agent/internal/runner"
	"github.com/JuanBaez77/status-sv/agent/internal/unithelper"
	"github.com/JuanBaez77/status-sv/internal/projectspec"
	"github.com/JuanBaez77/status-sv/proto"
)

// runDeployPaths imprime, por cada proyecto declarado y válido, su nombre, su
// workdir y su runtime, separados por un espacio. Lo usa el instalador para
// escribir ReadWritePaths en el drop-in del deploy y para saber si hace falta
// Docker o el ayudante de systemd.
func runDeployPaths(_ context.Context, args []string) error {
	flags := flag.NewFlagSet("deploy-paths", flag.ContinueOnError)
	dir := flags.String("projects-dir", "", "directorio de proyectos (o STATUSSV_PROJECTS_DIR)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *dir != "" {
		cfg.ProjectsDir = *dir
	}
	return printDeployPaths(os.Stdout, os.Stderr, projects.Read(cfg.ProjectsDir, time.Now()))
}

func printDeployPaths(out, warn io.Writer, state proto.ProjectsState) error {
	if state.Source.Status == proto.SourceError {
		return fmt.Errorf("no se pudo leer el directorio de proyectos: %s", state.Source.Detail)
	}
	for _, p := range state.Projects {
		if dropin.Hidden(p.Workdir) {
			_, _ = fmt.Fprintf(warn, "aviso: %s está en %s, que la unidad esconde (ProtectHome); no se va a poder desplegar\n", p.Name, p.Workdir)
			continue
		}
		_, _ = fmt.Fprintf(out, "%s %s %s\n", p.Name, p.Workdir, p.Runtime)
	}
	for _, invalid := range state.Invalid {
		_, _ = fmt.Fprintf(warn, "aviso: %s es inválido y queda afuera: %s\n", invalid.File, invalid.Error)
	}
	return nil
}

// runDeployCheck revisa, sin tocar lo que corre, que un proyecto se pueda
// desplegar: la declaración, git, la llave y los permisos del workdir; con
// Docker, el archivo de Compose y el acceso a Docker; con systemd, que el
// ayudante responda y que el usuario y las unidades del proyecto existan. Trae
// la rama declarada (eso solo escribe refs/remotes/statussv), así se prueba
// también la deploy key.
//
// Si el workdir todavía no tiene el código, revisa que se pueda hacer el
// primer deploy (ADR-021): ahí no trae nada, solo le pregunta al repo por la
// punta de la rama.
//
// Se corre como el usuario del agent, con sus grupos (el de Docker, si el
// servidor lo tiene):
//
//	sudo runuser -u statussv -g statussv -G docker -- statussv-agent deploy-check tienda
func runDeployCheck(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("deploy-check", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("uso: agent deploy-check PROYECTO")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	c := &checker{out: os.Stdout}
	c.run(ctx, cfg, flags.Arg(0))
	if c.failures > 0 {
		return fmt.Errorf("%d comprobaciones fallaron: el deploy de %s no va a andar hasta arreglarlas", c.failures, flags.Arg(0))
	}
	if c.first {
		_, _ = fmt.Fprintf(c.out, "\n%s todavía no se desplegó: está listo para su primer deploy", flags.Arg(0))
	} else {
		_, _ = fmt.Fprintf(c.out, "\n%s se puede desplegar", flags.Arg(0))
	}
	if !cfg.DeployEnabled {
		_, _ = fmt.Fprint(c.out, " cuando se prenda el deploy (STATUSSV_DEPLOY_ENABLED)")
	}
	_, _ = fmt.Fprintln(c.out, ".")
	return nil
}

type checker struct {
	out      io.Writer
	failures int
	// first es que el proyecto todavía no tiene el código en su workdir: lo
	// que se revisa es que se pueda hacer el primer deploy (ADR-021).
	first bool
}

func (c *checker) ok(format string, args ...any) {
	_, _ = fmt.Fprintf(c.out, "OK     "+format+"\n", args...)
}

func (c *checker) fail(format string, args ...any) {
	c.failures++
	_, _ = fmt.Fprintf(c.out, "FALLA  "+format+"\n", args...)
}

func (c *checker) warn(format string, args ...any) {
	_, _ = fmt.Fprintf(c.out, "AVISO  "+format+"\n", args...)
}

func (c *checker) run(ctx context.Context, cfg config.Config, name string) {
	spec, err := projects.ReadSpec(cfg.ProjectsDir, name)
	if err != nil {
		c.fail("declaración en %s: %v", cfg.ProjectsDir, err)
		return
	}
	p := spec.Project
	c.ok("declaración: %s, rama %s, %s (%s)", p.Repo, p.Branch, p.Workdir, p.Runtime)
	if dropin.Hidden(p.Workdir) {
		c.fail("el workdir %s está en un directorio que la unidad esconde (ProtectHome)", p.Workdir)
		return
	}

	gitPath, err := exec.LookPath("git")
	if err != nil {
		c.fail("git no está instalado")
		return
	}
	stateDir := filepath.Dir(cfg.StateFile)

	key := filepath.Join(cfg.DeployKeysDir, p.Name)
	info, err := os.Lstat(key)
	switch {
	case err != nil:
		key = ""
		c.warn("sin deploy key en %s: el repo se trae por HTTPS, así que tiene que ser público", filepath.Join(cfg.DeployKeysDir, p.Name))
	case !info.Mode().IsRegular():
		c.fail("la deploy key %s no es un archivo regular", key)
		return
	case info.Mode().Perm()&0o077 != 0:
		c.fail("la deploy key %s tiene permisos %o: ssh la rechaza si no es 0400 o 0600", key, info.Mode().Perm())
		return
	default:
		f, err := os.Open(key) //nolint:gosec // ruta armada con un nombre de proyecto validado
		if err != nil {
			c.fail("el agent no puede leer la deploy key %s", key)
			return
		}
		_ = f.Close()
		c.ok("deploy key %s", key)
		if _, err := os.Stat(cfg.KnownHosts); err != nil {
			c.fail("falta %s con los host keys del servidor de git", cfg.KnownHosts)
			return
		}
	}

	url, err := gitops.FetchURL(p.Repo, key != "", cfg.AllowFileRepos)
	if err != nil {
		c.fail("dirección del repo: %v", err)
		return
	}
	repo, err := gitops.Open(gitops.Config{Git: gitPath, StateDir: stateDir, KnownHosts: cfg.KnownHosts, AllowFileRepos: cfg.AllowFileRepos}, p.Workdir, key)
	if err != nil {
		c.fail("%v", err)
		return
	}
	cloned, err := repo.Cloned()
	if err != nil {
		c.fail("workdir %s: %v (lo crea una persona, con permiso de escritura para el agent)", p.Workdir, err)
		return
	}
	head, err := repo.Head(ctx)
	if !cloned || errors.Is(err, gitops.ErrNoCommit) {
		c.first = true
		c.firstDeploy(ctx, cfg, spec, repo, url, stateDir)
		return
	}
	if err != nil {
		c.fail("repositorio en %s: %v", p.Workdir, err)
		return
	}
	c.ok("repositorio en %s, en %s", p.Workdir, head[:8])
	if err := repo.CheckClean(ctx); err != nil {
		c.fail("%v", err)
	} else {
		c.ok("sin archivos versionados cambiados a mano")
	}
	for _, dir := range []string{p.Workdir, filepath.Join(p.Workdir, ".git")} {
		if err := writable(dir); err != nil {
			c.fail("el agent no puede escribir en %s: %v", dir, err)
		} else {
			c.ok("puede escribir en %s", dir)
		}
	}
	tail := runner.NewTail(3, 200)
	if err := repo.Fetch(ctx, url, p.Branch, tail); err != nil {
		c.fail("traer %s desde %s: %v %s", p.Branch, url, err, strings.Join(tail.Lines(), " | "))
	} else if tip, err := repo.Resolve(ctx, p.Branch, ""); err != nil {
		c.fail("leer la punta de %s: %v", p.Branch, err)
	} else {
		c.ok("trae %s desde %s: la punta está en %s", p.Branch, url, tip[:8])
	}

	if p.Runtime == proto.RuntimeSystemd {
		c.systemd(ctx, cfg, spec)
		return
	}
	c.docker(ctx, cfg, spec, stateDir, true)
}

// firstDeploy revisa un proyecto que todavía no tiene el código en su workdir
// (ADR-021): que el agent pueda escribir ahí, que el repo conteste (así se
// prueba la deploy key, sin traer nada) y lo del runtime que no depende del
// código. El archivo de Compose se valida recién en el primer deploy, cuando
// existe.
func (c *checker) firstDeploy(ctx context.Context, cfg config.Config, spec projectspec.Spec, repo *gitops.Repo, url, stateDir string) {
	p := spec.Project
	c.ok("%s todavía no tiene el código: le toca un primer deploy", p.Workdir)
	if err := writable(p.Workdir); err != nil {
		c.fail("el agent no puede escribir en %s: %v", p.Workdir, err)
	} else {
		c.ok("puede escribir en %s", p.Workdir)
	}
	if tip, err := repo.RemoteTip(ctx, url, p.Branch); err != nil {
		c.fail("consultar %s en %s: %v", p.Branch, url, err)
	} else {
		c.ok("el repo contesta desde %s: la punta de %s está en %s", url, p.Branch, tip[:8])
	}

	if p.Runtime == proto.RuntimeSystemd {
		c.systemd(ctx, cfg, spec)
		return
	}
	c.docker(ctx, cfg, spec, stateDir, false)
}

// docker revisa lo propio de un proyecto de Docker: Compose y el socket. Sin
// el código en el workdir (cloned en falso) no hay archivo de Compose que
// validar.
func (c *checker) docker(ctx context.Context, cfg config.Config, spec projectspec.Spec, stateDir string, cloned bool) {
	p := spec.Project
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		c.fail("docker no está instalado")
		return
	}
	project, err := compose.New(compose.Config{Docker: dockerPath, Socket: cfg.DockerSocket, StateDir: stateDir}, p)
	if err != nil {
		c.fail("%v", err)
		return
	}
	version, err := project.Version(ctx)
	if err != nil {
		c.fail("%v", err)
		return
	}
	c.ok("docker compose %s", version)
	if cloned {
		validate := runner.NewTail(3, 200)
		if err := project.Validate(ctx, validate); err != nil {
			c.fail("el archivo de Compose no es válido: %s", strings.Join(validate.Lines(), " | "))
		} else {
			c.ok("archivo de Compose %s", p.ComposeFile)
		}
	}
	containers, err := docker.NewClient(cfg.DockerSocket).ProjectContainers(ctx, p.ComposeProject)
	switch {
	case err != nil:
		c.fail("socket de Docker: %v", err)
	case !cloned && len(containers) > 0:
		c.fail("el proyecto %s ya tiene %d contenedores y su workdir no tiene el código: un primer deploy se va a negar", p.ComposeProject, len(containers))
	default:
		c.ok("socket de Docker: %d contenedores del proyecto %s", len(containers), p.ComposeProject)
	}
	for _, s := range append(append([]projectspec.Step{}, spec.Pre...), spec.Post...) {
		c.ok("paso declarado en %s: %s", s.Service, strings.Join(s.Command, " "))
	}
	if len(spec.Pre) > 0 {
		c.warn("los pasos pre no se deshacen si el deploy vuelve atrás (por ejemplo, una migración)")
	}
}

// systemd revisa lo propio de un proyecto de systemd (ADR-019): que el
// ayudante responda y diga que el usuario, el archivo de entorno y las
// unidades existen, y qué pasos va a correr.
func (c *checker) systemd(ctx context.Context, cfg config.Config, spec projectspec.Spec) {
	p := spec.Project
	err := unithelper.NewClient(cfg.UnitHelperSocket).Check(ctx, p.Name)
	var helperErr *unithelper.Error
	switch {
	case errors.Is(err, unithelper.ErrUnavailable):
		c.fail("el ayudante de deploy no está en %s: se instala con install-agent.sh --enable-deploy", cfg.UnitHelperSocket)
		return
	case errors.As(err, &helperErr):
		c.fail("el ayudante dice que no está listo: %s", helperErr.Message)
	case err != nil:
		c.fail("el ayudante no contestó: %v", err)
		return
	default:
		c.ok("el ayudante responde: existen las unidades %s", strings.Join(p.Units, ", "))
		if spec.RunAs != "" {
			c.ok("los pasos corren como %s", spec.RunAs)
		}
	}

	phases := []struct {
		name  string
		steps []projectspec.Step
	}{{"build", spec.Build}, {"pre", spec.Pre}, {"post", spec.Post}}
	for _, phase := range phases {
		for _, s := range phase.steps {
			c.ok("paso %s: %s", phase.name, strings.Join(s.Command, " "))
			// Puede no existir todavía y que lo cree un paso anterior (el
			// entorno virtual, por ejemplo): es un aviso. Antes del primer
			// deploy no existe ninguno: el código todavía no está.
			if _, err := os.Stat(s.Exec); err != nil && !c.first {
				c.warn("el programa %s no existe o el agent no lo ve; si no lo crea un paso anterior, el deploy va a fallar ahí", s.Exec)
			}
		}
	}
	if len(spec.Pre) > 0 {
		c.warn("los pasos pre no se deshacen si el deploy vuelve atrás (por ejemplo, una migración)")
	}
	if len(spec.Build) > 0 {
		c.warn("volver atrás corre los pasos build de nuevo sobre el commit anterior: conviene fijar las versiones de las dependencias")
	}
}

// writable prueba escribir un archivo en dir y lo borra.
func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".statussv-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// runDeployDropin imprime el drop-in 20-deploy.conf con los workdir de los
// proyectos declarados. Lo usa el instalador, que lo escribe en su lugar: así
// queda igual que el que deja `project accept` (ADR-022).
func runDeployDropin(_ context.Context, args []string) error {
	flags := flag.NewFlagSet("deploy-dropin", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	state := projects.Read(cfg.ProjectsDir, time.Now())
	if state.Source.Status == proto.SourceError {
		return fmt.Errorf("no se pudo leer el directorio de proyectos: %s", state.Source.Detail)
	}
	content, warnings := dropin.Render(state)
	for _, w := range warnings {
		_, _ = fmt.Fprintln(os.Stderr, "aviso: "+w)
	}
	_, err = fmt.Fprint(os.Stdout, content)
	return err
}
