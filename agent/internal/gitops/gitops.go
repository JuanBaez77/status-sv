// Package gitops trae el código de un proyecto declarado y lo deja en el
// commit que se despliega (ADR-015).
//
// git corre endurecido desde la línea de comando, que pisa la configuración
// del repo: ningún hook del workdir se ejecuta, no hay ayudantes de
// credenciales ni fsmonitor, solo se habla https y ssh, y safe.directory se
// fija al workdir (que es de root). El código sale siempre de la URL que se
// arma desde el repo declarado, nunca del origin del workdir, y por SSH con la
// deploy key del proyecto.
package gitops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/runner"
	"github.com/JuanBaez77/status-sv/proto"
)

// Tiempos máximos de cada operación (ADR-015).
const (
	FetchTimeout    = 2 * time.Minute
	CheckoutTimeout = 2 * time.Minute
	queryTimeout    = 30 * time.Second
)

// remoteRef es donde queda la punta de la rama que se trajo. Es un espacio de
// nombres propio: no pisa las ramas remotas de quien clonó el repo.
const remoteRefPrefix = "refs/remotes/statussv/"

// Errores que distinguen por qué no se puede desplegar.
var (
	ErrNotRepository = errors.New("el workdir no es un repositorio de git")
	// ErrNoWorkdir es que el directorio del proyecto no existe.
	ErrNoWorkdir = errors.New("el workdir no existe o no es un directorio")
	// ErrNoCommit es un repositorio recién iniciado, sin ningún commit: lo
	// que deja un primer deploy que no llegó a traer el código.
	ErrNoCommit      = errors.New("el repositorio del workdir todavía no tiene ningún commit")
	ErrDirty         = errors.New("hay archivos versionados cambiados a mano en el workdir")
	ErrNotOnBranch   = errors.New("el commit pedido no está en la rama declarada")
	ErrUnknownCommit = errors.New("el commit pedido no existe en el repositorio")
	ErrRepoURL       = errors.New("no se puede armar la dirección del repo")
)

// Config es lo que el agent sabe de antemano, igual para todos los proyectos.
type Config struct {
	// Git es la ruta absoluta de git.
	Git string
	// StateDir es el directorio de estado del agent, que hace de HOME.
	StateDir string
	// KnownHosts es el archivo de host keys conocidos para SSH.
	KnownHosts string
	// AllowFileRepos acepta repos file:// (solo para las pruebas de punta a
	// punta: en un servidor, el código viene de la red).
	AllowFileRepos bool
}

// Repo es el workdir de un proyecto.
type Repo struct {
	cfg     Config
	workdir string
	// key es la deploy key del proyecto; vacía si no tiene.
	key string
}

// safePath son los caracteres admitidos en las rutas que terminan adentro de
// GIT_SSH_COMMAND, que git pasa por un shell: sin espacios ni nada que un
// shell interprete.
var safePath = regexp.MustCompile(`^/[A-Za-z0-9/._-]+$`)

// Open prepara el workdir de un proyecto. key es la ruta de su deploy key, o
// vacía si el repo es público.
func Open(cfg Config, workdir, key string) (*Repo, error) {
	if !strings.HasPrefix(cfg.Git, "/") && !strings.Contains(cfg.Git, ":") {
		return nil, fmt.Errorf("git tiene que ser una ruta absoluta: %q", cfg.Git)
	}
	for _, p := range []string{key, cfg.KnownHosts} {
		if p != "" && !safePath.MatchString(p) {
			return nil, fmt.Errorf("ruta con caracteres no admitidos: %q", p)
		}
	}
	return &Repo{cfg: cfg, workdir: workdir, key: key}, nil
}

// FetchURL arma la dirección desde la que se trae el código, a partir del repo
// declarado (ADR-015):
//
//   - github.com/dueño/repo: por SSH si hay deploy key, por HTTPS si no.
//   - git@github.com:dueño/repo.git o ssh://...: por SSH; necesita llave.
//   - https://...: tal cual, sin usuario ni contraseña.
//   - file://...: solo si la configuración lo permite (pruebas).
func FetchURL(repo string, hasKey, allowFile bool) (string, error) {
	switch {
	case strings.HasPrefix(repo, "https://"):
		u, err := url.Parse(repo)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("%w: una dirección https no puede llevar usuario, contraseña ni parámetros", ErrRepoURL)
		}
		return repo, nil
	case strings.HasPrefix(repo, "ssh://"):
		u, err := url.Parse(repo)
		if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("%w: dirección ssh inválida", ErrRepoURL)
		}
		if _, hasPassword := u.User.Password(); hasPassword {
			return "", fmt.Errorf("%w: una dirección ssh no puede llevar contraseña", ErrRepoURL)
		}
		if !hasKey {
			return "", fmt.Errorf("%w: el repo es por SSH y el proyecto no tiene deploy key", ErrRepoURL)
		}
		return repo, nil
	case strings.HasPrefix(repo, "file://"):
		if !allowFile {
			return "", fmt.Errorf("%w: los repos file:// solo se aceptan en pruebas", ErrRepoURL)
		}
		return repo, nil
	case strings.Contains(repo, "://"):
		return "", fmt.Errorf("%w: solo se aceptan https y ssh", ErrRepoURL)
	}

	// Forma scp: git@host:dueño/repo.git.
	if user, rest, ok := strings.Cut(repo, "@"); ok {
		host, path, ok := strings.Cut(rest, ":")
		if user == "" || !ok || !validHost(host) || !validRepoPath(path) {
			return "", fmt.Errorf("%w: se esperaba usuario@servidor:dueño/repo", ErrRepoURL)
		}
		if !hasKey {
			return "", fmt.Errorf("%w: el repo es por SSH y el proyecto no tiene deploy key", ErrRepoURL)
		}
		return "ssh://" + user + "@" + host + "/" + withGit(path), nil
	}

	// Forma corta: servidor/dueño/repo.
	host, path, ok := strings.Cut(repo, "/")
	if !ok || !validHost(host) || !validRepoPath(path) {
		return "", fmt.Errorf("%w: se esperaba servidor/dueño/repo", ErrRepoURL)
	}
	if hasKey {
		return "ssh://git@" + host + "/" + withGit(path), nil
	}
	return "https://" + host + "/" + withGit(path), nil
}

var (
	hostPattern     = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?$`)
	repoPathPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)+$`)
)

func validHost(h string) bool { return hostPattern.MatchString(h) && strings.Contains(h, ".") }

func validRepoPath(p string) bool {
	return repoPathPattern.MatchString(p) && !strings.Contains(p, "..")
}

func withGit(path string) string {
	if strings.HasSuffix(path, ".git") {
		return path
	}
	return path + ".git"
}

// RemoteRef es la referencia donde queda la punta de la rama traída.
func RemoteRef(branch string) string {
	return remoteRefPrefix + branch
}

// command arma la invocación de git con el endurecimiento de ADR-015.
func (r *Repo) command(timeout time.Duration, args ...string) runner.Command {
	protocols := []string{"-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.ssh.allow=always"}
	if r.cfg.AllowFileRepos {
		protocols = append(protocols, "-c", "protocol.file.allow=always")
	}
	hardening := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "credential.helper=",
		"-c", "submodule.recurse=false",
		"-c", "safe.directory=" + r.workdir,
		"-C", r.workdir,
	}
	full := append(append(protocols, hardening...), args...)

	env := append(runner.BaseEnv(r.cfg.StateDir),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_SSH_COMMAND="+r.sshCommand(),
	)
	return runner.Command{Path: r.cfg.Git, Args: full, Dir: r.workdir, Env: env, Timeout: timeout}
}

// sshCommand es el ssh que usa git: sin configuración del sistema, sin
// preguntar nada, solo con la llave del proyecto y los host keys conocidos.
func (r *Repo) sshCommand() string {
	parts := []string{"ssh", "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15"}
	if r.cfg.KnownHosts != "" {
		parts = append(parts, "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+r.cfg.KnownHosts)
	}
	if r.key != "" {
		parts = append(parts, "-i", r.key, "-o", "IdentitiesOnly=yes")
	}
	return strings.Join(parts, " ")
}

// query corre un comando de consulta y devuelve su primer renglón.
func (r *Repo) query(ctx context.Context, args ...string) (string, error) {
	out, err := runner.Output(ctx, r.command(queryTimeout, args...), nil)
	return runner.FirstLine(out), err
}

// Head devuelve el commit en el que está el workdir.
func (r *Repo) Head(ctx context.Context) (string, error) {
	inside, err := r.query(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil || inside != "true" {
		return "", ErrNotRepository
	}
	sha, err := r.query(ctx, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	var exit *runner.ExitError
	if errors.As(err, &exit) && exit.Code == 1 {
		// Con --quiet, git sale con 1 y sin decir nada cuando HEAD no apunta
		// a ningún commit.
		return "", ErrNoCommit
	}
	if err != nil {
		return "", fmt.Errorf("leer el commit actual: %w", err)
	}
	if !proto.ValidCommit(sha) {
		return "", fmt.Errorf("leer el commit actual: respuesta inesperada de git")
	}
	return sha, nil
}

// Cloned dice si el workdir ya tiene un repositorio propio. Se mira el
// directorio, no se le pregunta a git: un workdir vacío adentro de otro
// repositorio no cuenta como clonado. Si el workdir no existe devuelve
// ErrNoWorkdir: el agent no lo crea.
func (r *Repo) Cloned() (bool, error) {
	info, err := os.Stat(r.workdir)
	switch {
	case errors.Is(err, fs.ErrNotExist) || (err == nil && !info.IsDir()):
		return false, ErrNoWorkdir
	case err != nil:
		return false, fmt.Errorf("mirar %s: %w", r.workdir, err)
	}
	_, err = os.Lstat(filepath.Join(r.workdir, ".git"))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("mirar %s: %w", r.workdir, err)
	}
}

// Init inicia un repositorio vacío en el workdir, para el primer deploy
// (ADR-021). No copia la plantilla de git, que trae hooks de ejemplo, ni
// configura ningún origin: el código se trae siempre de la URL que se arma
// desde el repo declarado. Sobre un repositorio que ya existe no cambia nada.
func (r *Repo) Init(ctx context.Context, tail *runner.Tail) error {
	return runner.Run(ctx, r.command(queryTimeout, "init", "--quiet", "--template="), tail)
}

// RemoteTip le pregunta al servidor de git por la punta de branch, sin traer
// nada ni necesitar un repositorio. Sirve para probar la deploy key de un
// proyecto que todavía no se clonó.
func (r *Repo) RemoteTip(ctx context.Context, fetchURL, branch string) (string, error) {
	out, err := runner.Output(ctx, r.command(FetchTimeout, "ls-remote", "--heads", "--", fetchURL, "refs/heads/"+branch), nil)
	if err != nil {
		return "", err
	}
	sha, _, _ := strings.Cut(runner.FirstLine(out), "\t")
	if !proto.ValidCommit(sha) {
		return "", fmt.Errorf("el repo no tiene la rama %s", branch)
	}
	return sha, nil
}

// CheckClean verifica que no haya archivos versionados cambiados. Los que no
// están versionados (un .env, los datos de un volumen) no cuentan.
func (r *Repo) CheckClean(ctx context.Context) error {
	out, err := runner.Output(ctx, r.command(queryTimeout, "status", "--porcelain=v1", "--untracked-files=no"), nil)
	if err != nil {
		return fmt.Errorf("revisar el árbol de trabajo: %w", err)
	}
	if n := countLines(out); n > 0 {
		return fmt.Errorf("%w (%d)", ErrDirty, n)
	}
	return nil
}

// Fetch trae la punta de branch desde fetchURL a RemoteRef(branch).
func (r *Repo) Fetch(ctx context.Context, fetchURL, branch string, tail *runner.Tail) error {
	refspec := "+refs/heads/" + branch + ":" + RemoteRef(branch)
	return runner.Run(ctx, r.command(FetchTimeout,
		"fetch", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--", fetchURL, refspec), tail)
}

// Resolve devuelve el commit que se despliega: la punta traída de branch si
// commit está vacío, o commit si existe y es ancestro de esa punta.
func (r *Repo) Resolve(ctx context.Context, branch, commit string) (string, error) {
	tip, err := r.query(ctx, "rev-parse", "--verify", RemoteRef(branch)+"^{commit}")
	if err != nil || !proto.ValidCommit(tip) {
		return "", fmt.Errorf("leer la punta de %s: %w", branch, errOr(err))
	}
	if commit == "" {
		return tip, nil
	}
	if !proto.ValidCommit(commit) {
		return "", ErrUnknownCommit
	}
	if _, err := r.query(ctx, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return "", ErrUnknownCommit
	}
	err = runner.Run(ctx, r.command(queryTimeout, "merge-base", "--is-ancestor", commit, tip), nil)
	var exit *runner.ExitError
	switch {
	case err == nil:
		return commit, nil
	case errors.As(err, &exit) && exit.Code == 1:
		return "", ErrNotOnBranch
	default:
		return "", fmt.Errorf("verificar que el commit esté en %s: %w", branch, err)
	}
}

// HasCommit dice si el commit existe en el repositorio (el anterior, para
// volver atrás).
func (r *Repo) HasCommit(ctx context.Context, commit string) bool {
	if !proto.ValidCommit(commit) {
		return false
	}
	_, err := r.query(ctx, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// Checkout deja el workdir en sha, con la rama local branch apuntando ahí:
// así un git log en el servidor dice la verdad.
func (r *Repo) Checkout(ctx context.Context, branch, sha string, tail *runner.Tail) error {
	if !proto.ValidCommit(sha) {
		return ErrUnknownCommit
	}
	return runner.Run(ctx, r.command(CheckoutTimeout,
		"checkout", "--quiet", "--no-recurse-submodules", "-B", branch, sha, "--"), tail)
}

func countLines(b []byte) int {
	n := 0
	for line := range strings.SplitSeq(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func errOr(err error) error {
	if err == nil {
		return errors.New("respuesta inesperada de git")
	}
	return err
}
