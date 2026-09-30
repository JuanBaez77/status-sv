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
	"github.com/JuanBaez77/status-sv/agent/internal/gitops"
	"github.com/JuanBaez77/status-sv/agent/internal/projects"
	"github.com/JuanBaez77/status-sv/agent/internal/projectspec"
	"github.com/JuanBaez77/status-sv/agent/internal/runner"
	"github.com/JuanBaez77/status-sv/proto"
)

// hiddenPrefixes son los directorios que ProtectHome=true esconde del agent:
// un workdir ahí no se puede desplegar (ADR-015).
var hiddenPrefixes = []string{"/home/", "/root/", "/run/user/"}

// runDeployPaths imprime, por cada proyecto de Docker declarado y válido, su
// nombre y su workdir, separados por un espacio. Lo usa el instalador para
// escribir ReadWritePaths en el drop-in del deploy.
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
		if p.Runtime != proto.RuntimeDocker {
			continue
		}
		if hidden(p.Workdir) {
			_, _ = fmt.Fprintf(warn, "aviso: %s está en %s, que la unidad esconde (ProtectHome); no se va a poder desplegar\n", p.Name, p.Workdir)
			continue
		}
		_, _ = fmt.Fprintf(out, "%s %s\n", p.Name, p.Workdir)
	}
	for _, invalid := range state.Invalid {
		_, _ = fmt.Fprintf(warn, "aviso: %s es inválido y queda afuera: %s\n", invalid.File, invalid.Error)
	}
	return nil
}

func hidden(workdir string) bool {
	for _, prefix := range hiddenPrefixes {
		if strings.HasPrefix(workdir+"/", prefix) {
			return true
		}
	}
	return false
}

// runDeployCheck revisa, sin tocar lo que corre, que un proyecto se pueda
// desplegar: la declaración, git, la llave, los permisos del workdir, el
// archivo de Compose y el acceso a Docker. Trae la rama declarada (eso solo
// escribe refs/remotes/statussv), así se prueba también la deploy key.
//
// Se corre como el usuario del agent: sudo runuser -u statussv -- statussv-agent deploy-check tienda
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
	_, _ = fmt.Fprintf(c.out, "\n%s se puede desplegar", flags.Arg(0))
	if !cfg.DeployEnabled {
		_, _ = fmt.Fprint(c.out, " cuando se prenda el deploy (STATUSSV_DEPLOY_ENABLED)")
	}
	_, _ = fmt.Fprintln(c.out, ".")
	return nil
}

type checker struct {
	out      io.Writer
	failures int
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
	c.ok("declaración: %s, rama %s, %s", p.Repo, p.Branch, p.Workdir)
	if p.Runtime != proto.RuntimeDocker {
		c.fail("runtime %s: el deploy de systemd llega en el Sprint 5", p.Runtime)
		return
	}
	if hidden(p.Workdir) {
		c.fail("el workdir %s está en un directorio que la unidad esconde (ProtectHome)", p.Workdir)
		return
	}

	gitPath, err := exec.LookPath("git")
	if err != nil {
		c.fail("git no está instalado")
		return
	}
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		c.fail("docker no está instalado")
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
	head, err := repo.Head(ctx)
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
	validate := runner.NewTail(3, 200)
	if err := project.Validate(ctx, validate); err != nil {
		c.fail("el archivo de Compose no es válido: %s", strings.Join(validate.Lines(), " | "))
	} else {
		c.ok("archivo de Compose %s", p.ComposeFile)
	}
	containers, err := docker.NewClient(cfg.DockerSocket).ProjectContainers(ctx, p.ComposeProject)
	if err != nil {
		c.fail("socket de Docker: %v", err)
	} else {
		c.ok("socket de Docker: %d contenedores del proyecto %s", len(containers), p.ComposeProject)
	}
	for _, s := range append(append([]projectspec.Step{}, spec.Pre...), spec.Post...) {
		c.ok("paso declarado en %s: %s", s.Service, strings.Join(s.Command, " "))
	}
	if len(spec.Pre) > 0 {
		c.warn("los pasos pre no se deshacen si el deploy vuelve atrás (por ejemplo, una migración)")
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
