package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/config"
	"github.com/JuanBaez77/status-sv/agent/internal/gitops"
	"github.com/JuanBaez77/status-sv/internal/projectspec"
	"github.com/JuanBaez77/status-sv/proto"
)

func TestPrintDeployPaths(t *testing.T) {
	state := proto.ProjectsState{
		TakenAt: time.Now(),
		Source:  proto.RuntimeSource{Status: proto.SourceOK},
		Projects: []proto.Project{
			{Name: "tienda", Runtime: proto.RuntimeDocker, Workdir: "/srv/tienda"},
			{Name: "facturas", Runtime: proto.RuntimeSystemd, Workdir: "/srv/facturas"},
			{Name: "casa", Runtime: proto.RuntimeDocker, Workdir: "/home/juan/casa"},
			{Name: "raiz", Runtime: proto.RuntimeDocker, Workdir: "/root"},
		},
		Invalid: []proto.InvalidProject{{File: "roto.yaml", Error: "name: falta"}},
	}
	var out, warn bytes.Buffer
	if err := printDeployPaths(&out, &warn, state); err != nil {
		t.Fatal(err)
	}
	// Todos los que se pueden desplegar, con su runtime, y nada que ProtectHome
	// esconda.
	if out.String() != "tienda /srv/tienda docker\nfacturas /srv/facturas systemd\n" {
		t.Errorf("salida = %q", out.String())
	}
	for _, want := range []string{"casa", "raiz", "roto.yaml"} {
		if !strings.Contains(warn.String(), want) {
			t.Errorf("falta el aviso de %s: %q", want, warn.String())
		}
	}

	if err := printDeployPaths(&out, &warn, proto.ProjectsState{Source: proto.RuntimeSource{Status: proto.SourceError, Detail: "permiso denegado"}}); err == nil {
		t.Error("con el directorio ilegible tendría que fallar: el instalador no puede escribir un drop-in a ciegas")
	}
}

// Sin el ayudante instalado, deploy-check de un proyecto de systemd dice qué
// falta y cómo se arregla, en vez de un error de conexión.
func TestCheckSystemdWithoutHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("un socket que no existe da otro error en Windows; el agent corre en Linux")
	}
	var out bytes.Buffer
	c := &checker{out: &out}
	cfg := config.Config{UnitHelperSocket: filepath.Join(t.TempDir(), "no-hay-socket")}
	spec := projectspec.Spec{Project: proto.Project{Name: "facturas", Runtime: proto.RuntimeSystemd, Units: []string{"gunicorn-facturas.service"}}}

	c.systemd(t.Context(), cfg, spec)

	if c.failures != 1 || !strings.Contains(out.String(), "install-agent.sh --enable-deploy") {
		t.Errorf("fallas = %d, salida = %q", c.failures, out.String())
	}
}

// Un proyecto cuyo workdir todavía no tiene el código no está roto: le toca
// un primer deploy (ADR-021). deploy-check prueba que el agent pueda escribir
// ahí y que el repo conteste, sin traer nada.
func TestCheckBeforeTheFirstDeploy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("el agent corre en Linux")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no hay git")
	}
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(gitPath, append([]string{"-c", "user.name=prueba", "-c", "user.email=prueba@status-sv.invalid", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+t.TempDir())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	origin := t.TempDir()
	git(origin, "init", "-q")
	git(origin, "commit", "-q", "--allow-empty", "-m", "v1")

	workdir := t.TempDir()
	cfg := config.Config{AllowFileRepos: true, UnitHelperSocket: filepath.Join(t.TempDir(), "no-hay-socket")}
	spec := projectspec.Spec{Project: proto.Project{
		Name: "facturas", Runtime: proto.RuntimeSystemd, Workdir: workdir, Branch: "main", Units: []string{"gunicorn-facturas.service"},
	}}
	repo, err := gitops.Open(gitops.Config{Git: gitPath, StateDir: t.TempDir(), AllowFileRepos: true}, workdir, "")
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	c := &checker{out: &out, first: true}
	c.firstDeploy(t.Context(), cfg, spec, repo, "file://"+origin, t.TempDir())

	for _, want := range []string{"le toca un primer deploy", "puede escribir en " + workdir, "el repo contesta", "la punta de main"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("falta %q en la salida:\n%s", want, out.String())
		}
	}
	// Lo único que falla es el ayudante, que acá no está.
	if c.failures != 1 || !strings.Contains(out.String(), "install-agent.sh --enable-deploy") {
		t.Errorf("fallas = %d, salida:\n%s", c.failures, out.String())
	}
	// No trajo nada: el workdir sigue sin repositorio.
	if cloned, err := repo.Cloned(); err != nil || cloned {
		t.Errorf("deploy-check dejó un repositorio en el workdir: %v, %v", cloned, err)
	}
	if entries, _ := os.ReadDir(workdir); len(entries) != 0 {
		t.Errorf("deploy-check dejó archivos en el workdir: %v", entries)
	}
}

// Los host keys hacen falta solo si el repo se trae por SSH: un proyecto con
// deploy key y un repo que no es SSH no los pide.
func TestCheckKnownHostsOnlyForSSH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("el agent corre en Linux")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no hay git")
	}
	projectsDir, keysDir, workdir := t.TempDir(), t.TempDir(), t.TempDir()
	declare := func(repo string) {
		t.Helper()
		yaml := "name: facturas\nenvironment: staging\nrepo: " + repo + "\nbranch: main\nruntime: systemd\nworkdir: " + workdir +
			"\nunits: [gunicorn-facturas.service]\n"
		if err := os.WriteFile(filepath.Join(projectsDir, "facturas.yaml"), []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(keysDir, "facturas"), []byte("llave"), 0o400); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ProjectsDir: projectsDir, DeployKeysDir: keysDir, AllowFileRepos: true,
		KnownHosts:       filepath.Join(t.TempDir(), "no-hay-known_hosts"),
		StateFile:        filepath.Join(t.TempDir(), "agent.json"),
		UnitHelperSocket: filepath.Join(t.TempDir(), "no-hay-socket"),
	}

	declare("file://" + t.TempDir())
	var out bytes.Buffer
	c := &checker{out: &out}
	c.run(t.Context(), cfg, "facturas")
	if strings.Contains(out.String(), "host keys") {
		t.Errorf("pidió host keys para un repo file://:\n%s", out.String())
	}

	declare("github.com/a/facturas")
	out.Reset()
	c = &checker{out: &out}
	c.run(t.Context(), cfg, "facturas")
	if !strings.Contains(out.String(), "con los host keys del servidor de git") {
		t.Errorf("no pidió host keys para un repo por SSH:\n%s", out.String())
	}
}
