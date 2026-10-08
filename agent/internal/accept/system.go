package accept

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"time"
)

// OS es el sistema de verdad. Corre los mismos programas que el instalador
// (deploy/install-agent.sh), con argumentos armados acá, nunca con texto de la
// propuesta que no haya pasado el parser.
type OS struct {
	// Binary es el agent, para correr deploy-check.
	Binary string
	// StateFile y ProjectsDir viajan a deploy-check como en el instalador.
	StateFile   string
	ProjectsDir string
	// Out recibe lo que dicen los programas.
	Out io.Writer
}

// IsRoot implementa System.
func (OS) IsRoot() bool { return os.Geteuid() == 0 }

// UserExists implementa System.
func (OS) UserExists(name string) (bool, error) {
	_, err := user.Lookup(name)
	var unknown user.UnknownUserError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &unknown):
		return false, nil
	}
	return false, fmt.Errorf("buscar el usuario %s: %w", name, err)
}

// CreateUser implementa System: un usuario del sistema con su grupo, sin
// shell, con home en el workdir (ahí deja pip su caché) y sin crearlo.
func (o OS) CreateUser(name, home string) error {
	return o.run(context.Background(), "useradd", "--system", "--user-group", "--no-create-home",
		"--home-dir", home, "--shell", "/usr/sbin/nologin", "--", name)
}

// Chown implementa System.
func (o OS) Chown(path, owner, group string) error {
	uid, gid, err := o.Owner(owner, group)
	if err != nil {
		return err
	}
	return os.Lchown(path, uid, gid)
}

// Keygen implementa System.
func (o OS) Keygen(path, comment string) error {
	return o.run(context.Background(), "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", path)
}

// Hostname implementa System.
func (OS) Hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "servidor"
	}
	return name
}

// RestartAgent implementa System, y espera a que el agent quede andando.
func (o OS) RestartAgent(ctx context.Context) error {
	if err := o.run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := o.run(ctx, "systemctl", "restart", "statussv-agent"); err != nil {
		return err
	}
	for range 10 {
		time.Sleep(time.Second)
		if exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "statussv-agent").Run() == nil {
			return nil
		}
	}
	return errors.New("el agent no quedó corriendo: mirá journalctl -u statussv-agent")
}

// DeployCheck implementa System: como el instalador, con el usuario del agent
// y sus grupos (el del diario y, si existe, el de Docker). El -g hace falta:
// sin él, runuser usa el primer -G como grupo primario.
func (o OS) DeployCheck(ctx context.Context, project string) error {
	args := []string{"-u", AgentUser, "-g", AgentUser, "-G", "systemd-journal"}
	if _, err := user.LookupGroup("docker"); err == nil {
		args = append(args, "-G", "docker")
	}
	args = append(args, "--", "env",
		"STATUSSV_STATE_FILE="+o.StateFile,
		"STATUSSV_PROJECTS_DIR="+o.ProjectsDir,
		"STATUSSV_DEPLOY_ENABLED=true",
	)
	// Para el laboratorio y la prueba de punta a punta, que usan un repo
	// file://; como en el instalador, solo si viene del entorno.
	if v := os.Getenv("STATUSSV_DEPLOY_ALLOW_FILE_REPOS"); v != "" {
		args = append(args, "STATUSSV_DEPLOY_ALLOW_FILE_REPOS="+v)
	}
	args = append(args, o.Binary, "deploy-check", project)
	return o.run(ctx, "runuser", args...)
}

func (o OS) run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // programas fijos, argumentos validados
	cmd.Stdout, cmd.Stderr = o.Out, o.Out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// Owner implementa System.
func (OS) Owner(owner, group string) (int, int, error) {
	u, err := user.Lookup(owner)
	if err != nil {
		return 0, 0, fmt.Errorf("buscar el usuario %s: %w", owner, err)
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return 0, 0, fmt.Errorf("buscar el grupo %s: %w", group, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

// DaemonReload implementa System.
func (o OS) DaemonReload(ctx context.Context) error {
	return o.run(ctx, "systemctl", "daemon-reload")
}

// VerifyUnit implementa System. systemd-analyze verify sale con 0 aunque la
// unidad tenga problemas: lo que importa es lo que dice.
func (OS) VerifyUnit(ctx context.Context, path string) (string, error) {
	out, err := exec.CommandContext(ctx, "systemd-analyze", "verify", "--man=no", path).CombinedOutput() //nolint:gosec // ruta armada con un nombre validado
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return "", err
	}
	return string(out), nil
}

// Has implementa System.
func (OS) Has(program string) bool {
	_, err := exec.LookPath(program)
	return err == nil
}

// Resolves implementa System.
func (OS) Resolves(domain string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, domain)
	return err == nil && len(addrs) > 0
}

// NginxTest implementa System.
func (o OS) NginxTest(ctx context.Context) error {
	return o.run(ctx, "nginx", "-t")
}

// NginxReload implementa System.
func (o OS) NginxReload(ctx context.Context) error {
	return o.run(ctx, "systemctl", "reload", "nginx")
}

// Certbot implementa System.
func (o OS) Certbot(ctx context.Context, args []string) error {
	return o.run(ctx, "certbot", args...)
}
