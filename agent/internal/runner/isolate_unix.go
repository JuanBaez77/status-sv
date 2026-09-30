//go:build unix

package runner

import (
	"os/exec"
	"syscall"
)

// isolate pone al programa en su propio grupo de procesos y, si hay que
// cortarlo, mata el grupo entero: docker compose corre como plugin de docker,
// y matar solo al primero dejaría al segundo andando.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
