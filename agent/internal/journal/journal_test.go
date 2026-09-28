package journal

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Renglones con el formato de journalctl --output=json (systemd 255), con
// los campos que se usan y alguno de más.
const sample = `{"__REALTIME_TIMESTAMP":"1790565850679987","PRIORITY":"6","SYSLOG_IDENTIFIER":"cron","MESSAGE":"(root) CMD (backup.sh)"}
{"__REALTIME_TIMESTAMP":"1790565851000000","PRIORITY":"3","MESSAGE":"error: no se pudo abrir /var/log/app"}
{"__REALTIME_TIMESTAMP":"1790565852000000","PRIORITY":"6","MESSAGE":[104,111,108,97,255]}
esto no es JSON
{"__REALTIME_TIMESTAMP":"x","MESSAGE":"sin prioridad ni fecha"}
`

// fakeRun devuelve una salida fija y guarda con qué argumentos se lo llamó.
func fakeRun(output string, waitErr error, got *[]string) func(context.Context, string, []string) (io.ReadCloser, func() error, error) {
	return func(_ context.Context, path string, args []string) (io.ReadCloser, func() error, error) {
		*got = append([]string{path}, args...)
		return io.NopCloser(strings.NewReader(output)), func() error { return waitErr }, nil
	}
}

func TestTail(t *testing.T) {
	var args []string
	r := &Reader{path: "/usr/bin/journalctl", run: fakeRun(sample, nil, &args)}

	entries, err := r.Tail(context.Background(), "cron.service", 50)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}

	// La unidad va pegada a su opción, en un solo argumento, y no hay shell.
	want := "/usr/bin/journalctl --unit=cron.service --lines=50 --output=json --no-pager --quiet"
	if strings.Join(args, " ") != want {
		t.Errorf("argumentos:\n %s\nesperaba:\n %s", strings.Join(args, " "), want)
	}

	if len(entries) != 4 {
		t.Fatalf("esperaba 4 renglones (el que no es JSON se saltea), hay %d: %+v", len(entries), entries)
	}
	if !entries[0].TS.Equal(time.UnixMicro(1790565850679987).UTC()) || entries[0].Priority != 6 || entries[0].Message != "(root) CMD (backup.sh)" {
		t.Errorf("primer renglón: %+v", entries[0])
	}
	if entries[1].Priority != 3 {
		t.Errorf("prioridad de error: %+v", entries[1])
	}
	// journald manda como bytes un mensaje que no es UTF-8 válido.
	if entries[2].Message != "hola\xff" {
		t.Errorf("mensaje en bytes: %q", entries[2].Message)
	}
	if !entries[3].TS.IsZero() || entries[3].Priority != 6 {
		t.Errorf("sin fecha ni prioridad: %+v", entries[3])
	}
}

func TestTailWithoutJournalctl(t *testing.T) {
	r := &Reader{}
	if _, err := r.Tail(context.Background(), "cron.service", 10); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("esperaba ErrUnavailable, obtuve %v", err)
	}
}

// Aunque la unidad llegue validada, el lector no confía: nada que empiece con
// un guion llega a journalctl.
func TestTailRejectsOptionLikeUnits(t *testing.T) {
	var args []string
	r := &Reader{path: "/usr/bin/journalctl", run: fakeRun("", nil, &args)}
	for _, unit := range []string{"", "-f", "--since=ayer.service"} {
		if _, err := r.Tail(context.Background(), unit, 10); err == nil {
			t.Errorf("la unidad %q no tendría que llegar a journalctl", unit)
		}
	}
	if args != nil {
		t.Errorf("se ejecutó journalctl con %v", args)
	}
}

func TestTailReportsFailures(t *testing.T) {
	var args []string
	r := &Reader{path: "/usr/bin/journalctl", run: fakeRun("", errors.New("exit status 1"), &args)}
	if _, err := r.Tail(context.Background(), "cron.service", 10); err == nil {
		t.Fatal("un journalctl que falla sin salida es un error")
	}
}

// Contra el journalctl de verdad, si la máquina que corre el test lo tiene.
func TestRealJournal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("journald es de Linux")
	}
	r := NewReader()
	if r.path == "" {
		t.Skip("no hay journalctl en esta máquina")
	}
	if _, err := os.Stat("/run/systemd/journal"); err != nil {
		t.Skip("journald no está corriendo")
	}
	entries, err := r.Tail(context.Background(), "systemd-journald.service", 5)
	if err != nil {
		t.Fatalf("Tail contra el diario de verdad: %v", err)
	}
	t.Logf("%d renglones del diario de systemd-journald", len(entries))
}
