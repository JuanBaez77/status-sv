//go:build unix

package gitops

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JuanBaez77/status-sv/agent/internal/runner"
)

// fixture es un origen con dos commits en main y un workdir clonado en el
// primero, como el de un servidor antes de desplegar.
type fixture struct {
	origin  string
	workdir string
	v1, v2  string
	repo    *Repo
}

// plainGit corre git sin endurecer, como lo haría una persona.
func plainGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{"-c", "user.name=prueba", "-c", "user.email=prueba@status-sv.invalid", "-c", "init.defaultBranch=main", "-c", "protocol.file.allow=always"}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no hay git")
	}

	src := t.TempDir()
	plainGit(t, src, "init", "-q")
	writeFile(t, filepath.Join(src, "index.html"), "v1\n")
	plainGit(t, src, "add", "-A")
	plainGit(t, src, "commit", "-qm", "v1")
	v1 := plainGit(t, src, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(src, "nuevo.html"), "v2\n")
	plainGit(t, src, "add", "-A")
	plainGit(t, src, "commit", "-qm", "v2")
	v2 := plainGit(t, src, "rev-parse", "HEAD")

	origin := filepath.Join(t.TempDir(), "origen.git")
	plainGit(t, src, "clone", "-q", "--bare", src, origin)

	workdir := filepath.Join(t.TempDir(), "proyecto")
	plainGit(t, filepath.Dir(workdir), "clone", "-q", "file://"+origin, workdir)
	plainGit(t, workdir, "checkout", "-q", "-B", "main", v1)

	cfg := Config{Git: gitPath, StateDir: t.TempDir(), AllowFileRepos: true}
	repo, err := Open(cfg, workdir, "")
	if err != nil {
		t.Fatal(err)
	}
	return fixture{origin: origin, workdir: workdir, v1: v1, v2: v2, repo: repo}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec // archivo de prueba
		t.Fatal(err)
	}
}

func TestDeployFlow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	head, err := f.repo.Head(ctx)
	if err != nil || head != f.v1 {
		t.Fatalf("Head = %q, %v; se esperaba v1", head, err)
	}
	if err := f.repo.CheckClean(ctx); err != nil {
		t.Fatalf("CheckClean: %v", err)
	}

	tail := runner.NewTail(20, 200)
	if err := f.repo.Fetch(ctx, "file://"+f.origin, "main", tail); err != nil {
		t.Fatalf("Fetch: %v (%v)", err, tail.Lines())
	}

	tip, err := f.repo.Resolve(ctx, "main", "")
	if err != nil || tip != f.v2 {
		t.Fatalf("Resolve de la punta = %q, %v; se esperaba v2", tip, err)
	}
	if got, err := f.repo.Resolve(ctx, "main", f.v1); err != nil || got != f.v1 {
		t.Fatalf("Resolve de v1 = %q, %v", got, err)
	}

	// Un hook plantado en el workdir no se ejecuta.
	marker := filepath.Join(t.TempDir(), "hook-corrio")
	hook := filepath.Join(f.workdir, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil { //nolint:gosec // el hook tiene que ser ejecutable
		t.Fatal(err)
	}

	if err := f.repo.Checkout(ctx, "main", f.v2, tail); err != nil {
		t.Fatalf("Checkout: %v (%v)", err, tail.Lines())
	}
	if head, _ := f.repo.Head(ctx); head != f.v2 {
		t.Fatalf("Head después del checkout = %q", head)
	}
	if branch := plainGit(t, f.workdir, "rev-parse", "--abbrev-ref", "HEAD"); branch != "main" {
		t.Errorf("la rama local quedó en %q y no en main", branch)
	}
	if _, err := os.Stat(filepath.Join(f.workdir, "nuevo.html")); err != nil {
		t.Errorf("el archivo de v2 no está: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("corrió el hook post-checkout del workdir")
	}

	// Volver atrás es otro checkout.
	if !f.repo.HasCommit(ctx, f.v1) {
		t.Fatal("HasCommit(v1) = false")
	}
	if err := f.repo.Checkout(ctx, "main", f.v1, tail); err != nil {
		t.Fatalf("Checkout de vuelta: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.workdir, "nuevo.html")); !os.IsNotExist(err) {
		t.Errorf("el archivo de v2 sigue después de volver: %v", err)
	}
}

func TestCheckCleanIgnoresUntracked(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	writeFile(t, filepath.Join(f.workdir, ".env"), "SECRETO=1\n")
	if err := f.repo.CheckClean(ctx); err != nil {
		t.Fatalf("un archivo sin versionar no tendría que contar: %v", err)
	}

	writeFile(t, filepath.Join(f.workdir, "index.html"), "arreglo a mano\n")
	if err := f.repo.CheckClean(ctx); !errors.Is(err, ErrDirty) {
		t.Fatalf("CheckClean con un cambio a mano = %v, se esperaba ErrDirty", err)
	}
}

func TestResolveRejectsCommitsOutsideTheBranch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.repo.Fetch(ctx, "file://"+f.origin, "main", nil); err != nil {
		t.Fatal(err)
	}

	// Un commit que existe en el repo pero no en la rama declarada.
	plainGit(t, f.workdir, "checkout", "-q", "-b", "otra")
	writeFile(t, filepath.Join(f.workdir, "otra.html"), "x\n")
	plainGit(t, f.workdir, "add", "-A")
	plainGit(t, f.workdir, "commit", "-qm", "fuera de main")
	outside := plainGit(t, f.workdir, "rev-parse", "HEAD")

	if _, err := f.repo.Resolve(ctx, "main", outside); !errors.Is(err, ErrNotOnBranch) {
		t.Errorf("Resolve de un commit fuera de main = %v, se esperaba ErrNotOnBranch", err)
	}
	unknown := strings.Repeat("ab", 20)
	if _, err := f.repo.Resolve(ctx, "main", unknown); !errors.Is(err, ErrUnknownCommit) {
		t.Errorf("Resolve de un commit que no existe = %v, se esperaba ErrUnknownCommit", err)
	}
}

func TestHeadOutsideARepository(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no hay git")
	}
	repo, err := Open(Config{Git: gitPath, StateDir: t.TempDir()}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Head(context.Background()); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("Head fuera de un repo = %v", err)
	}
}

// El primer deploy (ADR-021) arranca de un workdir sin clonar, donde una
// persona pudo haber dejado un .env: se inicia el repositorio, se trae la rama
// y el checkout no pisa lo que ya estaba.
func TestFirstDeployFlow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	workdir := t.TempDir()
	writeFile(t, filepath.Join(workdir, ".env"), "SECRETO=1\n")
	repo, err := Open(f.repo.cfg, workdir, "")
	if err != nil {
		t.Fatal(err)
	}

	if cloned, err := repo.Cloned(); err != nil || cloned {
		t.Fatalf("Cloned en un workdir sin clonar = %v, %v", cloned, err)
	}
	// El workdir lo crea una persona: si no está, no se inicia nada.
	missing, err := Open(f.repo.cfg, filepath.Join(workdir, "no-existe"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.Cloned(); !errors.Is(err, ErrNoWorkdir) {
		t.Fatalf("Cloned sin workdir = %v, se esperaba ErrNoWorkdir", err)
	}
	// La punta se puede preguntar sin repositorio: así se prueba la llave.
	if tip, err := repo.RemoteTip(ctx, "file://"+f.origin, "main"); err != nil || tip != f.v2 {
		t.Fatalf("RemoteTip = %q, %v; se esperaba v2", tip, err)
	}
	if _, err := repo.RemoteTip(ctx, "file://"+f.origin, "no-existe"); err == nil {
		t.Error("RemoteTip de una rama que no existe no falló")
	}

	tail := runner.NewTail(20, 200)
	if err := repo.Init(ctx, tail); err != nil {
		t.Fatalf("Init: %v (%v)", err, tail.Lines())
	}
	if cloned, err := repo.Cloned(); err != nil || !cloned {
		t.Fatalf("Cloned después de Init = %v, %v", cloned, err)
	}
	// La plantilla de git no se copia: no hay hooks, ni de ejemplo.
	if entries, err := os.ReadDir(filepath.Join(workdir, ".git", "hooks")); err == nil && len(entries) > 0 {
		t.Errorf("Init dejó %d hooks de la plantilla", len(entries))
	}
	if _, err := repo.Head(ctx); !errors.Is(err, ErrNoCommit) {
		t.Fatalf("Head de un repositorio recién iniciado = %v, se esperaba ErrNoCommit", err)
	}
	// Iniciar de nuevo (un intento anterior que no llegó a traer nada) no rompe.
	if err := repo.Init(ctx, tail); err != nil {
		t.Fatalf("Init por segunda vez: %v", err)
	}

	if err := repo.Fetch(ctx, "file://"+f.origin, "main", tail); err != nil {
		t.Fatalf("Fetch: %v (%v)", err, tail.Lines())
	}
	tip, err := repo.Resolve(ctx, "main", "")
	if err != nil || tip != f.v2 {
		t.Fatalf("Resolve = %q, %v; se esperaba v2", tip, err)
	}
	if err := repo.Checkout(ctx, "main", tip, tail); err != nil {
		t.Fatalf("Checkout: %v (%v)", err, tail.Lines())
	}
	if head, err := repo.Head(ctx); err != nil || head != f.v2 {
		t.Fatalf("Head = %q, %v; se esperaba v2", head, err)
	}
	if branch := plainGit(t, workdir, "rev-parse", "--abbrev-ref", "HEAD"); branch != "main" {
		t.Errorf("la rama local quedó en %q y no en main", branch)
	}
	if env, err := os.ReadFile(filepath.Join(workdir, ".env")); err != nil || string(env) != "SECRETO=1\n" { //nolint:gosec // archivo de prueba
		t.Errorf("el .env que estaba de antes = %q, %v", env, err)
	}
	if err := repo.CheckClean(ctx); err != nil {
		t.Errorf("CheckClean después del primer checkout: %v", err)
	}
}

// Si en el workdir ya hay un archivo con el nombre de uno versionado, el
// primer checkout falla en vez de pisarlo.
func TestFirstCheckoutDoesNotOverwrite(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	workdir := t.TempDir()
	writeFile(t, filepath.Join(workdir, "index.html"), "lo que había\n")
	repo, err := Open(f.repo.cfg, workdir, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Init(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := repo.Fetch(ctx, "file://"+f.origin, "main", nil); err != nil {
		t.Fatal(err)
	}

	tail := runner.NewTail(20, 200)
	if err := repo.Checkout(ctx, "main", f.v2, tail); err == nil {
		t.Fatal("el checkout pisó un archivo que no era del repositorio")
	}
	if got, _ := os.ReadFile(filepath.Join(workdir, "index.html")); string(got) != "lo que había\n" { //nolint:gosec // archivo de prueba
		t.Errorf("index.html = %q", got)
	}
}

// Sin permiso para file://, git no trae nada de ahí: protocol.allow=never.
func TestFetchRefusesFileWithoutPermission(t *testing.T) {
	f := newFixture(t)
	f.repo.cfg.AllowFileRepos = false
	err := f.repo.Fetch(context.Background(), "file://"+f.origin, "main", nil)
	var exit *runner.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("Fetch por file:// sin permiso = %v, se esperaba que git se negara", err)
	}
}
