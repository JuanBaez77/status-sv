package commands

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/agent/internal/docker"
	"github.com/JuanBaez77/status-sv/agent/internal/journal"
	"github.com/JuanBaez77/status-sv/proto"
)

var (
	containerID = strings.Repeat("ab", 32)
	t0          = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
)

type fakeDocker struct {
	mu     sync.Mutex
	lines  []docker.LogLine
	err    error
	calls  int
	block  chan struct{}
	gotTTY bool
}

func (f *fakeDocker) Inspect(context.Context, string) (docker.Inspection, error) {
	var i docker.Inspection
	i.Config.Tty = true
	return i, f.err
}

func (f *fakeDocker) Logs(_ context.Context, _ string, _ int, tty bool) ([]docker.LogLine, error) {
	f.mu.Lock()
	f.calls++
	f.gotTTY = tty
	block := f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return f.lines, f.err
}

type fakeJournal struct {
	entries []journal.Entry
	err     error
}

func (f *fakeJournal) Tail(context.Context, string, int) ([]journal.Entry, error) {
	return f.entries, f.err
}

func knowsAll(proto.LogKind, string) bool { return true }

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func logsCmd(kind proto.LogKind, target string, lines int) proto.Command {
	return proto.Command{Operation: proto.OpGetLogs, GetLogs: &proto.GetLogsParams{Kind: kind, Target: target, Lines: lines}}
}

func TestContainerLogsAreSanitized(t *testing.T) {
	d := &fakeDocker{lines: []docker.LogLine{
		{TS: t0, Stream: "stdout", Text: "\x1b[32mINFO\x1b[0m arrancando"},
		{TS: t0.Add(time.Second), Stream: "stderr", Text: "conectando a postgres://app:Cl4ve@db/prod"},
		{TS: t0.Add(2 * time.Second), Stream: "stdout", Text: "columna\tcon\ttabs"},
	}}
	e := NewExecutor(d, nil, knowsAll, quietLog())
	id := uuid.New()

	ack := e.Handle(context.Background(), id, logsCmd(proto.LogContainer, containerID, 100))
	if ack.Status != proto.CommandOK || ack.CommandID != id {
		t.Fatalf("respuesta: %+v", ack)
	}
	if err := ack.Validate(); err != nil {
		t.Fatalf("la respuesta no pasa el contrato: %v", err)
	}
	got := ack.Logs.Lines
	if got[0].Text != "INFO arrancando" {
		t.Errorf("las secuencias de escape tienen que salir: %q", got[0].Text)
	}
	if strings.Contains(got[1].Text, "Cl4ve") || got[1].Stream != "stderr" {
		t.Errorf("el secreto tiene que salir enmascarado: %+v", got[1])
	}
	if got[2].Text != "columna con tabs" {
		t.Errorf("los tabs son espacios: %q", got[2].Text)
	}
	if !d.gotTTY {
		t.Error("el formato de los logs depende de si el contenedor tiene terminal")
	}
}

func TestServiceLogsMarkErrors(t *testing.T) {
	j := &fakeJournal{entries: []journal.Entry{
		{TS: t0, Priority: 6, Message: "iniciado"},
		{TS: t0.Add(time.Second), Priority: 3, Message: "falló la conexión"},
	}}
	e := NewExecutor(nil, j, knowsAll, quietLog())
	ack := e.Handle(context.Background(), uuid.New(), logsCmd(proto.LogService, "nginx.service", 10))
	if ack.Status != proto.CommandOK || len(ack.Logs.Lines) != 2 {
		t.Fatalf("respuesta: %+v", ack)
	}
	if ack.Logs.Lines[0].Stream != "stdout" || ack.Logs.Lines[1].Stream != "stderr" {
		t.Errorf("prioridad de error como stderr: %+v", ack.Logs.Lines)
	}
}

// Solo se opera sobre lo que el agent vio en su última foto (regla 2).
func TestUnknownTargetIsRejected(t *testing.T) {
	d := &fakeDocker{}
	e := NewExecutor(d, nil, func(proto.LogKind, string) bool { return false }, quietLog())
	ack := e.Handle(context.Background(), uuid.New(), logsCmd(proto.LogContainer, containerID, 10))
	if ack.Status != proto.CommandRejected || ack.Code != proto.CommandUnknownTarget {
		t.Fatalf("respuesta: %+v", ack)
	}
	if d.calls != 0 {
		t.Error("no tendría que haberle preguntado nada a Docker")
	}
}

func TestErrorsAreMapped(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status proto.CommandStatus
		code   proto.CommandErrorCode
	}{
		{"contenedor borrado", docker.ErrNotFound, proto.CommandRejected, proto.CommandUnknownTarget},
		{"sin Docker", docker.ErrNoSocket, proto.CommandFailed, proto.CommandUnavailable},
		{"sin permiso", docker.ErrPermissionDenied, proto.CommandFailed, proto.CommandUnavailable},
		{"tiempo agotado", context.DeadlineExceeded, proto.CommandFailed, proto.CommandTimeout},
		{"otra cosa", errors.New("se rompió"), proto.CommandFailed, proto.CommandInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewExecutor(&fakeDocker{err: tt.err}, nil, knowsAll, quietLog())
			ack := e.Handle(context.Background(), uuid.New(), logsCmd(proto.LogContainer, containerID, 10))
			if ack.Status != tt.status || ack.Code != tt.code {
				t.Errorf("respuesta: %+v", ack)
			}
			if err := ack.Validate(); err != nil {
				t.Errorf("la respuesta no pasa el contrato: %v", err)
			}
		})
	}
}

func TestMissingSourcesAreUnavailable(t *testing.T) {
	e := NewExecutor(nil, nil, knowsAll, quietLog())
	for _, cmd := range []proto.Command{
		logsCmd(proto.LogContainer, containerID, 10),
		logsCmd(proto.LogService, "nginx.service", 10),
	} {
		ack := e.Handle(context.Background(), uuid.New(), cmd)
		if ack.Status != proto.CommandFailed || ack.Code != proto.CommandUnavailable {
			t.Errorf("%s: %+v", cmd.GetLogs.Kind, ack)
		}
	}
}

// Un comando repetido (el control plane no recibió la respuesta y lo reenvió)
// no se ejecuta dos veces: devuelve la misma respuesta.
func TestRepeatedCommandIsNotRunTwice(t *testing.T) {
	d := &fakeDocker{lines: []docker.LogLine{{TS: t0, Text: "hola"}}}
	e := NewExecutor(d, nil, knowsAll, quietLog())
	id := uuid.New()
	first := e.Handle(context.Background(), id, logsCmd(proto.LogContainer, containerID, 10))
	second := e.Handle(context.Background(), id, logsCmd(proto.LogContainer, containerID, 10))
	if d.calls != 1 {
		t.Errorf("Docker se consultó %d veces", d.calls)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Error("la segunda respuesta tiene que ser la misma")
	}
}

func TestBusy(t *testing.T) {
	d := &fakeDocker{block: make(chan struct{})}
	e := NewExecutor(d, nil, knowsAll, quietLog())

	var wg sync.WaitGroup
	for range maxConcurrent {
		wg.Go(func() { e.Handle(context.Background(), uuid.New(), logsCmd(proto.LogContainer, containerID, 10)) })
	}
	// Esperar a que los dos estén adentro.
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		calls := d.calls
		d.mu.Unlock()
		if calls == maxConcurrent || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	ack := e.Handle(context.Background(), uuid.New(), logsCmd(proto.LogContainer, containerID, 10))
	close(d.block)
	wg.Wait()
	if ack.Status != proto.CommandRejected || ack.Code != proto.CommandBusy {
		t.Errorf("con todos los lugares ocupados: %+v", ack)
	}
}

// Mil renglones enormes no entran en un mensaje: se dejan afuera los más
// viejos y se avisa.
func TestLargeLogsFitInAMessage(t *testing.T) {
	var lines []docker.LogLine
	for i := range proto.MaxLogLines {
		lines = append(lines, docker.LogLine{TS: t0.Add(time.Duration(i) * time.Second), Text: strings.Repeat("x", 3000)})
	}
	e := NewExecutor(&fakeDocker{lines: lines}, nil, knowsAll, quietLog())
	ack := e.Handle(context.Background(), uuid.New(), logsCmd(proto.LogContainer, containerID, proto.MaxLogLines))

	raw, err := proto.Marshal(ack)
	if err != nil {
		t.Fatalf("la respuesta no pasa el contrato: %v", err)
	}
	if len(raw) > proto.MaxMessageBytes {
		t.Errorf("la respuesta ocupa %d bytes", len(raw))
	}
	if !ack.Logs.Truncated || len(ack.Logs.Lines) == 0 {
		t.Fatalf("tendría que avisar que recortó: %d renglones", len(ack.Logs.Lines))
	}
	// Quedan los más nuevos.
	last := ack.Logs.Lines[len(ack.Logs.Lines)-1]
	if !last.TS.Equal(t0.Add(time.Duration(proto.MaxLogLines-1) * time.Second)) {
		t.Errorf("el último renglón tiene que ser el más nuevo: %v", last.TS)
	}
}

func TestRejectedExplainsUnreadableCommands(t *testing.T) {
	id := uuid.New()
	if ack := Rejected(id, proto.ErrUnknownOperation); ack.Code != proto.CommandUnknownOperation {
		t.Errorf("operación desconocida: %+v", ack)
	}
	if ack := Rejected(id, errors.New("contenedor inválido")); ack.Code != proto.CommandInvalidParams {
		t.Errorf("parámetros inválidos: %+v", ack)
	}
}

// fakeDeployer anota los deploys que le llegan.
type fakeDeployer struct {
	mu   sync.Mutex
	seen []uuid.UUID
}

func (f *fakeDeployer) Start(id uuid.UUID, _ proto.Command) proto.CommandAck {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, id)
	return proto.CommandAck{CommandID: id, Status: proto.CommandOK, Message: "deploy aceptado"}
}

func TestDeployGoesToTheEngine(t *testing.T) {
	d := &fakeDocker{block: make(chan struct{})}
	e := NewExecutor(d, nil, knowsAll, quietLog())
	deployCmd := proto.Command{Operation: proto.OpDeploy, Deploy: &proto.DeployParams{Project: "tienda"}}

	// Sin motor, no está disponible.
	if ack := e.Handle(context.Background(), uuid.New(), deployCmd); ack.Code != proto.CommandUnavailable {
		t.Fatalf("sin motor: %+v", ack)
	}

	deployer := &fakeDeployer{}
	e.SetDeployer(deployer)

	// Aunque los pedidos de logs ocupen todos los lugares, el deploy entra.
	var wg sync.WaitGroup
	for range maxConcurrent {
		wg.Go(func() { e.Handle(context.Background(), uuid.New(), logsCmd(proto.LogContainer, containerID, 10)) })
	}
	defer func() {
		close(d.block)
		wg.Wait()
	}()

	id := uuid.New()
	ack := e.Handle(context.Background(), id, deployCmd)
	if ack.Status != proto.CommandOK {
		t.Fatalf("deploy: %+v", ack)
	}
	rollback := e.Handle(context.Background(), uuid.New(), proto.Command{Operation: proto.OpRollback, Rollback: &proto.RollbackParams{Project: "tienda"}})
	if rollback.Status != proto.CommandOK {
		t.Fatalf("rollback: %+v", rollback)
	}
	// Repetido, contesta lo mismo sin volver a llamar al motor.
	e.Handle(context.Background(), id, deployCmd)
	deployer.mu.Lock()
	defer deployer.mu.Unlock()
	if len(deployer.seen) != 2 {
		t.Errorf("el motor recibió %d pedidos, se esperaban 2", len(deployer.seen))
	}
}
