// Package commands ejecuta los comandos que manda el control plane.
//
// Es la regla 2 del proyecto hecha código: el agent solo ejecuta operaciones de
// una lista cerrada, con parámetros tipados, y solo sobre lo que él mismo vio.
// get_logs se atiende acá; deploy y rollback se le pasan al motor de deploy
// (ADR-015), que los acepta o rechaza enseguida y los corre aparte.
package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/agent/internal/docker"
	"github.com/JuanBaez77/status-sv/agent/internal/journal"
	"github.com/JuanBaez77/status-sv/agent/internal/redact"
	"github.com/JuanBaez77/status-sv/proto"
)

const (
	// maxConcurrent es cuántos comandos corren a la vez. Un pedido de logs
	// cuesta poco, pero el agent tiene un techo de memoria y de procesos.
	maxConcurrent = 2
	// rememberedResults es cuántas respuestas se recuerdan para contestar
	// igual un comando repetido sin volver a ejecutarlo.
	rememberedResults = 32
	// envelopeMargin es lo que se reserva del tope de un mensaje para el
	// envelope que envuelve la respuesta.
	envelopeMargin = 1 << 10
)

// Las fuentes de logs, como las usa el ejecutor. Las cumplen docker.Client y
// journal.Reader; los tests las reemplazan.
type (
	containerLogs interface {
		Inspect(ctx context.Context, id string) (docker.Inspection, error)
		Logs(ctx context.Context, id string, lines int, tty bool) ([]docker.LogLine, error)
	}
	serviceLogs interface {
		Tail(ctx context.Context, unit string, lines int) ([]journal.Entry, error)
	}
)

// Deployer atiende deploy y rollback. Lo cumple deploy.Engine.
type Deployer interface {
	Start(id uuid.UUID, cmd proto.Command) proto.CommandAck
}

// Known dice si el agent vio ese contenedor o servicio en su última foto. Un
// comando sobre algo que el agent no conoce se rechaza, aunque el nombre sea
// válido.
type Known func(kind proto.LogKind, target string) bool

// Executor ejecuta comandos. Es seguro usarlo desde varias goroutines.
type Executor struct {
	docker  containerLogs
	journal serviceLogs
	known   Known
	deploy  Deployer
	log     *slog.Logger

	slots chan struct{}

	mu      sync.Mutex
	results map[uuid.UUID]proto.CommandAck
	order   []uuid.UUID
}

// NewExecutor arma el ejecutor. docker o journal pueden ser nil si el servidor
// no los tiene: los pedidos de ese tipo se contestan como no disponibles.
func NewExecutor(d containerLogs, j serviceLogs, known Known, log *slog.Logger) *Executor {
	return &Executor{
		docker:  d,
		journal: j,
		known:   known,
		log:     log,
		slots:   make(chan struct{}, maxConcurrent),
		results: make(map[uuid.UUID]proto.CommandAck),
	}
}

// SetDeployer le da al ejecutor el motor de deploy. Sin él, deploy y rollback
// se contestan como no disponibles (fuera de Linux, por ejemplo). Se llama
// antes de atender comandos.
func (e *Executor) SetDeployer(d Deployer) {
	e.deploy = d
}

// Handle ejecuta el comando id y devuelve la respuesta. Si el mismo comando ya
// se ejecutó (el control plane lo reenvió porque no le llegó la respuesta),
// devuelve la misma respuesta sin volver a ejecutarlo.
func (e *Executor) Handle(ctx context.Context, id uuid.UUID, cmd proto.Command) proto.CommandAck {
	if ack, ok := e.remembered(id); ok {
		return ack
	}

	// Un deploy no ocupa uno de los lugares de los pedidos de logs: el motor
	// contesta enseguida y lo corre aparte, de a uno.
	if cmd.Operation == proto.OpDeploy || cmd.Operation == proto.OpRollback {
		ack := reject(id, proto.CommandUnavailable, "este agent no tiene el motor de deploy")
		if e.deploy != nil {
			ack = e.deploy.Start(id, cmd)
		}
		e.remember(id, ack)
		return ack
	}

	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		return reject(id, proto.CommandBusy, "el agent ya está atendiendo otros comandos; reintentar en unos segundos")
	}

	var ack proto.CommandAck
	switch cmd.Operation {
	case proto.OpGetLogs:
		ack = e.getLogs(ctx, id, *cmd.GetLogs)
	default:
		// No pasa: el comando llega validado. Se deja para que agregar una
		// operación al contrato sin implementarla no ejecute nada por error.
		ack = reject(id, proto.CommandUnknownOperation, fmt.Sprintf("el agent no sabe ejecutar %q", cmd.Operation))
	}

	e.remember(id, ack)
	return ack
}

// Rejected arma la respuesta a un comando que no se pudo leer.
func Rejected(id uuid.UUID, err error) proto.CommandAck {
	if errors.Is(err, proto.ErrUnknownOperation) {
		return reject(id, proto.CommandUnknownOperation, "el agent no ejecuta esa operación")
	}
	return reject(id, proto.CommandInvalidParams, clean(err.Error(), 512))
}

func (e *Executor) getLogs(ctx context.Context, id uuid.UUID, p proto.GetLogsParams) proto.CommandAck {
	if !e.known(p.Kind, p.Target) {
		return reject(id, proto.CommandUnknownTarget, "el agent no ve eso en este servidor")
	}

	var lines []proto.LogLine
	switch p.Kind {
	case proto.LogContainer:
		if e.docker == nil {
			return fail(id, proto.CommandUnavailable, "este servidor no tiene Docker")
		}
		inspection, err := e.docker.Inspect(ctx, p.Target)
		if err != nil {
			return e.logsError(ctx, id, err)
		}
		raw, err := e.docker.Logs(ctx, p.Target, p.Lines, inspection.Config.Tty)
		if err != nil {
			return e.logsError(ctx, id, err)
		}
		for _, l := range raw {
			lines = append(lines, proto.LogLine{TS: l.TS, Stream: l.Stream, Text: sanitize(l.Text)})
		}
	case proto.LogService:
		if e.journal == nil {
			return fail(id, proto.CommandUnavailable, "este servidor no tiene journald")
		}
		entries, err := e.journal.Tail(ctx, p.Target, p.Lines)
		if err != nil {
			return e.logsError(ctx, id, err)
		}
		for _, entry := range entries {
			stream := "stdout"
			if entry.Priority <= 3 {
				stream = "stderr"
			}
			lines = append(lines, proto.LogLine{TS: entry.TS, Stream: stream, Text: sanitize(entry.Message)})
		}
	}

	if len(lines) > p.Lines {
		lines = lines[len(lines)-p.Lines:]
	}
	ack := proto.CommandAck{CommandID: id, Status: proto.CommandOK, Logs: &proto.LogsResult{Lines: lines}}
	fit(&ack)
	return ack
}

func (e *Executor) logsError(ctx context.Context, id uuid.UUID, err error) proto.CommandAck {
	switch {
	case errors.Is(err, docker.ErrNotFound):
		return reject(id, proto.CommandUnknownTarget, "el contenedor ya no existe")
	case errors.Is(err, docker.ErrNoSocket), errors.Is(err, docker.ErrPermissionDenied), errors.Is(err, journal.ErrUnavailable):
		return fail(id, proto.CommandUnavailable, clean(err.Error(), 512))
	case errors.Is(err, context.DeadlineExceeded):
		return fail(id, proto.CommandTimeout, "leer los logs tardó demasiado")
	default:
		e.log.WarnContext(ctx, "no se pudieron leer los logs", "error", err)
		return fail(id, proto.CommandInternal, clean(err.Error(), 512))
	}
}

// fit deja afuera los renglones más viejos hasta que la respuesta entre en un
// mensaje.
func fit(ack *proto.CommandAck) {
	for len(ack.Logs.Lines) > 0 {
		raw, err := json.Marshal(ack)
		if err != nil || len(raw) <= proto.MaxMessageBytes-envelopeMargin {
			return
		}
		drop := max(1, len(ack.Logs.Lines)/10)
		ack.Logs.Lines = ack.Logs.Lines[drop:]
		ack.Logs.Truncated = true
	}
}

func (e *Executor) remembered(id uuid.UUID) (proto.CommandAck, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ack, ok := e.results[id]
	return ack, ok
}

func (e *Executor) remember(id uuid.UUID, ack proto.CommandAck) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.results[id]; ok {
		return
	}
	e.results[id] = ack
	e.order = append(e.order, id)
	if len(e.order) > rememberedResults {
		delete(e.results, e.order[0])
		e.order = e.order[1:]
	}
}

func reject(id uuid.UUID, code proto.CommandErrorCode, message string) proto.CommandAck {
	return proto.CommandAck{CommandID: id, Status: proto.CommandRejected, Code: code, Message: message}
}

func fail(id uuid.UUID, code proto.CommandErrorCode, message string) proto.CommandAck {
	return proto.CommandAck{CommandID: id, Status: proto.CommandFailed, Code: code, Message: message}
}

// ansi son las secuencias de escape de terminal (colores, sobre todo), que
// muchos programas escriben en sus logs.
var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[@-Z\\-_]`)

// sanitize deja un renglón listo para salir del servidor: sin secuencias de
// escape, con los secretos enmascarados (regla 3) y recién después recortado,
// para que el recorte nunca deje media contraseña a la vista.
func sanitize(s string) string {
	s = ansi.ReplaceAllString(s, "")
	s = redact.String(s)
	return clean(s, proto.MaxLogLineLen)
}

// clean saca los caracteres de control y recorta a maxLen bytes sin cortar
// un carácter por la mitad.
func clean(s string, maxLen int) string {
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
	return s
}
