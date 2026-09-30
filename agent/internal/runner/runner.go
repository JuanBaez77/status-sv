// Package runner ejecuta los programas del motor de deploy (git y docker
// compose) con argumentos fijos, sin shell y con un entorno armado desde cero
// (ADR-015).
//
// Es el mismo criterio que journalctl en ADR-011: el programa va por ruta
// absoluta, los argumentos en una lista (nunca una cadena que alguien parsea) y
// cada valor llega validado de antes. El proceso hijo no hereda el entorno del
// agent: recibe solo lo que se le pasa.
//
// La salida se guarda acotada: de un paso largo (un build) interesan los
// últimos renglones, ya sin escapes de terminal y con los secretos
// enmascarados antes de recortarlos.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/JuanBaez77/status-sv/agent/internal/redact"
)

// SafePath es el PATH que reciben los procesos hijos. git lo usa para
// encontrar ssh y sus propios ayudantes; docker, sus plugins.
const SafePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// maxCapture acota lo que se guarda de la salida estándar cuando se la pide
// entera (un SHA, una lista corta). Lo que sobra se descarta.
const maxCapture = 256 << 10

// waitDelay es cuánto se espera a que se cierren las tuberías después de matar
// el proceso: un nieto que las heredó no puede dejar colgado al agent.
const waitDelay = 5 * time.Second

// ErrTimeout es que el programa se pasó de su tiempo y se lo mató.
var ErrTimeout = errors.New("se pasó de su tiempo máximo")

// ExitError es que el programa terminó con un código distinto de 0.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("terminó con código %d", e.Code)
}

// Command es un programa por ejecutar.
type Command struct {
	// Path es la ruta absoluta del programa.
	Path string
	// Args son los argumentos, sin el nombre del programa.
	Args []string
	// Dir es el directorio de trabajo; vacío es el del agent.
	Dir string
	// Env es el entorno completo del proceso: no se le agrega nada.
	Env []string
	// Timeout acota lo que puede tardar. Cero es sin más límite que el del
	// contexto.
	Timeout time.Duration
}

// BaseEnv es el entorno mínimo de un proceso hijo: PATH fijo, HOME en el
// directorio de estado del agent, idioma C (mensajes estables) y nada más.
func BaseEnv(home string) []string {
	return []string{"PATH=" + SafePath, "HOME=" + home, "LANG=C", "LC_ALL=C"}
}

// Output ejecuta el programa y devuelve su salida estándar, recortada. La
// salida de errores va a tail, si hay uno.
func Output(ctx context.Context, c Command, tail *Tail) ([]byte, error) {
	var stdout limitedBuffer
	err := run(ctx, c, &stdout, tail)
	return stdout.Bytes(), err
}

// Run ejecuta el programa y manda los renglones de las dos salidas a tail.
func Run(ctx context.Context, c Command, tail *Tail) error {
	return run(ctx, c, nil, tail)
}

func run(ctx context.Context, c Command, stdout io.Writer, tail *Tail) error {
	if c.Path == "" || !strings.HasPrefix(c.Path, "/") && !isWindowsAbs(c.Path) {
		return fmt.Errorf("el programa %q no es una ruta absoluta", c.Path)
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, c.Path, c.Args...) //nolint:gosec // programa fijo y argumentos validados (ADR-015)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	if cmd.Env == nil {
		// nil haría heredar el entorno del agent.
		cmd.Env = []string{}
	}
	cmd.WaitDelay = waitDelay
	isolate(cmd)

	lines := &lineWriter{tail: tail}
	cmd.Stderr = lines
	if stdout != nil {
		cmd.Stdout = stdout
	} else {
		cmd.Stdout = lines
	}

	err := cmd.Run()
	lines.flush()
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrTimeout
		}
		return ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &ExitError{Code: exitErr.ExitCode()}
	}
	if err != nil {
		return fmt.Errorf("ejecutar %s: %w", c.Path, err)
	}
	return nil
}

func isWindowsAbs(p string) bool {
	return len(p) > 2 && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

// Tail guarda los últimos renglones de salida, ya limpios. Es seguro usarlo
// desde varias goroutines.
type Tail struct {
	max    int
	maxLen int

	mu    sync.Mutex
	lines []string
}

// NewTail guarda los últimos n renglones, de hasta maxLen bytes cada uno.
func NewTail(n, maxLen int) *Tail {
	return &Tail{max: n, maxLen: maxLen}
}

// Add limpia un renglón y lo guarda. Los renglones vacíos no se guardan.
func (t *Tail) Add(line string) {
	if t == nil {
		return
	}
	line = Sanitize(line, t.maxLen)
	if strings.TrimSpace(line) == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
}

// Lines devuelve una copia de los renglones guardados, del más viejo al más
// nuevo.
func (t *Tail) Lines() []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.lines...)
}

// ansi son las secuencias de escape de terminal (colores, sobre todo).
var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[@-Z\\-_]`)

// Sanitize deja un renglón listo para salir del servidor: sin secuencias de
// escape, con los secretos enmascarados (regla 3) y recién después recortado a
// maxLen bytes, para que el recorte nunca deje media contraseña a la vista.
func Sanitize(s string, maxLen int) string {
	s = ansi.ReplaceAllString(s, "")
	s = redact.String(s)
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case unicode.IsControl(r):
			return -1
		default:
			return r
		}
	}, strings.ToValidUTF8(s, "?"))
	if len(s) > maxLen {
		s = strings.ToValidUTF8(s[:maxLen], "")
	}
	return strings.TrimRight(s, " ")
}

// maxLineBytes acota un renglón mientras se arma: una salida sin saltos de
// línea no puede llenar la memoria.
const maxLineBytes = 16 << 10

// lineWriter corta lo que escribe un proceso en renglones (por \n o por \r,
// que usan las barras de progreso) y los pasa a tail.
type lineWriter struct {
	tail *Tail

	mu      sync.Mutex
	partial []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b == '\n' || b == '\r' {
			w.emitLocked()
			continue
		}
		if len(w.partial) < maxLineBytes {
			w.partial = append(w.partial, b)
		}
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.emitLocked()
}

func (w *lineWriter) emitLocked() {
	if len(w.partial) > 0 {
		w.tail.Add(string(w.partial))
		w.partial = w.partial[:0]
	}
}

// limitedBuffer guarda hasta maxCapture bytes y descarta el resto sin fallar:
// que un programa escriba de más no es motivo para cortarlo.
type limitedBuffer struct {
	buf bytes.Buffer
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxCapture - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (l *limitedBuffer) Bytes() []byte { return l.buf.Bytes() }

// FirstLine devuelve el primer renglón de b, sin espacios alrededor.
func FirstLine(b []byte) string {
	line, _, _ := bytes.Cut(b, []byte("\n"))
	return strings.TrimSpace(string(line))
}
