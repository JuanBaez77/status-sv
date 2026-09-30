//go:build unix

package runner

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func sh(t *testing.T, script string) Command {
	t.Helper()
	path, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no hay sh")
	}
	return Command{Path: path, Args: []string{"-c", script}, Env: BaseEnv(t.TempDir())}
}

func TestOutputCapturesStdoutAndTailsStderr(t *testing.T) {
	tail := NewTail(10, 100)
	out, err := Output(context.Background(), sh(t, `echo sha123; echo "aviso" >&2`), tail)
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if FirstLine(out) != "sha123" {
		t.Errorf("stdout = %q", out)
	}
	if got := tail.Lines(); len(got) != 1 || got[0] != "aviso" {
		t.Errorf("tail = %q", got)
	}
}

func TestRunTailsBothStreams(t *testing.T) {
	tail := NewTail(3, 100)
	err := Run(context.Background(), sh(t, `for i in 1 2 3 4 5; do echo "renglón $i"; done; echo error >&2`), tail)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := tail.Lines()
	// Quedan los últimos tres, en orden.
	if len(got) != 3 || got[0] != "renglón 4" || got[1] != "renglón 5" || got[2] != "error" {
		t.Errorf("tail = %q", got)
	}
}

func TestExitCode(t *testing.T) {
	err := Run(context.Background(), sh(t, "exit 3"), nil)
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 3 {
		t.Fatalf("err = %v, se esperaba el código 3", err)
	}
}

// El hijo no hereda el entorno del agent: un secreto del proceso no le llega.
func TestEnvIsNotInherited(t *testing.T) {
	t.Setenv("STATUSSV_SECRETO_DEL_AGENT", "no-tendria-que-verse")
	out, err := Output(context.Background(), sh(t, `echo "[$STATUSSV_SECRETO_DEL_AGENT]"; echo "$PATH"`), nil)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if lines[0] != "[]" {
		t.Errorf("el hijo vio el entorno del agent: %q", lines[0])
	}
	if lines[1] != SafePath {
		t.Errorf("PATH = %q", lines[1])
	}
}

// Al pasarse de tiempo se mata el grupo entero, incluidos los nietos que
// tienen abiertas las tuberías, y la espera no se cuelga.
func TestTimeoutKillsTheProcessGroup(t *testing.T) {
	c := sh(t, "sleep 30 & sleep 30")
	c.Timeout = 300 * time.Millisecond
	start := time.Now()
	err := Run(context.Background(), c, NewTail(5, 100))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, se esperaba ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("tardó %s en cortar", elapsed)
	}
}

func TestRelativePathIsRejected(t *testing.T) {
	if err := Run(context.Background(), Command{Path: "git"}, nil); err == nil {
		t.Fatal("se aceptó un programa sin ruta absoluta")
	}
}

func TestProgressWithCarriageReturns(t *testing.T) {
	tail := NewTail(5, 100)
	if err := Run(context.Background(), sh(t, `printf '10%%\r50%%\r100%%\nlisto\n'`), tail); err != nil {
		t.Fatal(err)
	}
	got := tail.Lines()
	if strings.Join(got, "|") != "10%|50%|100%|listo" {
		t.Errorf("tail = %q", got)
	}
}
