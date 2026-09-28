// Package journal trae los últimos renglones del diario de un servicio de
// systemd, ejecutando journalctl con argumentos fijos (ADR-011).
//
// No hay shell en el medio: el programa es siempre journalctl, los argumentos
// van como una lista y cada valor llega validado. La unidad va pegada a su
// opción (--unit=nginx.service), así nada que empiece con un guion puede
// leerse como otra opción.
package journal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	// timeout acota lo que puede tardar journalctl.
	timeout = 10 * time.Second
	// maxOutputBytes acota lo que se lee de su salida.
	maxOutputBytes = 4 << 20
	// maxEntryBytes acota un renglón de JSON: uno más largo se saltea.
	maxEntryBytes = 256 << 10
)

// ErrUnavailable es que en este servidor no hay journalctl.
var ErrUnavailable = errors.New("no hay journalctl en este servidor")

// Entry es un renglón del diario.
type Entry struct {
	TS time.Time
	// Priority es la de syslog: 0 es emergencia, 3 error, 6 información.
	Priority int
	Message  string
}

// Reader ejecuta journalctl.
type Reader struct {
	path string
	// run reemplaza la ejecución en los tests.
	run func(ctx context.Context, path string, args []string) (io.ReadCloser, func() error, error)
}

// NewReader busca journalctl. Si no está, las lecturas devuelven ErrUnavailable.
func NewReader() *Reader {
	path, err := exec.LookPath("journalctl")
	if err != nil {
		path = ""
	}
	return &Reader{path: path, run: runCommand}
}

// Tail devuelve los últimos lines renglones de unit, del más viejo al más nuevo.
func (r *Reader) Tail(ctx context.Context, unit string, lines int) ([]Entry, error) {
	if r.path == "" {
		return nil, ErrUnavailable
	}
	if unit == "" || strings.HasPrefix(unit, "-") || lines < 1 {
		return nil, fmt.Errorf("pedido inválido: unidad %q, %d renglones", unit, lines)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"--unit=" + unit,
		"--lines=" + strconv.Itoa(lines),
		"--output=json",
		"--no-pager",
		"--quiet",
	}
	out, wait, err := r.run(ctx, r.path, args)
	if err != nil {
		return nil, fmt.Errorf("ejecutar journalctl: %w", err)
	}
	entries, parseErr := parse(io.LimitReader(out, maxOutputBytes))
	_ = out.Close()
	if err := wait(); err != nil && ctx.Err() == nil && parseErr == nil && len(entries) == 0 {
		return nil, fmt.Errorf("journalctl terminó con error: %w", err)
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("journalctl tardó más de %s: %w", timeout, ctx.Err())
	}
	return entries, parseErr
}

func runCommand(ctx context.Context, path string, args []string) (io.ReadCloser, func() error, error) {
	cmd := exec.CommandContext(ctx, path, args...) //nolint:gosec // programa fijo, argumentos validados (ADR-011)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return out, cmd.Wait, nil
}

// record es un renglón de journalctl --output=json, con los campos que se usan.
// MESSAGE puede venir como texto o, si tiene bytes que no son UTF-8, como un
// arreglo de números.
type record struct {
	Realtime string          `json:"__REALTIME_TIMESTAMP"`
	Priority string          `json:"PRIORITY"`
	Message  json.RawMessage `json:"MESSAGE"`
}

func parse(r io.Reader) ([]Entry, error) {
	var entries []Entry
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxEntryBytes)
	for scanner.Scan() {
		var rec record
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		entry := Entry{Priority: 6, Message: message(rec.Message)}
		if usec, err := strconv.ParseInt(rec.Realtime, 10, 64); err == nil {
			entry.TS = time.UnixMicro(usec).UTC()
		}
		if p, err := strconv.Atoi(rec.Priority); err == nil {
			entry.Priority = p
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return entries, fmt.Errorf("leer la salida de journalctl: %w", err)
	}
	return entries, nil
}

func message(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var bytes []byte
	var numbers []int
	if err := json.Unmarshal(raw, &numbers); err == nil {
		for _, n := range numbers {
			bytes = append(bytes, byte(n)) //nolint:gosec // journald manda bytes de 0 a 255
		}
		return string(bytes)
	}
	return ""
}
