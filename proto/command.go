package proto

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Operation es una operación que el control plane le pide al agent. Es una
// lista cerrada (regla 2 del proyecto): no existe una operación que ejecute
// texto libre, y el agent rechaza cualquier otra.
//
// Las de escritura (restart, stop, start…) se agregan a esta lista cuando
// existan, con sus parámetros tipados.
type Operation string

// Operaciones que existen.
const (
	// OpGetLogs trae los últimos renglones de log de un contenedor o de un
	// servicio.
	OpGetLogs Operation = "get_logs"
	// OpDeploy despliega un proyecto declarado (ADR-015). El acuse dice si
	// se aceptó; el avance llega después, en deploy_event.
	OpDeploy Operation = "deploy"
	// OpRollback vuelve un proyecto declarado a su último estado bueno.
	OpRollback Operation = "rollback"
)

// Límites de los comandos y sus respuestas.
const (
	// MaxLogLines es lo más que se puede pedir de una vez.
	MaxLogLines = 1000
	// MaxLogLineLen acota un renglón. Uno más largo se recorta en el agent,
	// después de enmascarar los secretos.
	MaxLogLineLen = 2048

	maxCommandMessageLen = 1024
)

// LogKind dice de qué es el log que se pide.
type LogKind string

// Tipos de log.
const (
	LogContainer LogKind = "container"
	LogService   LogKind = "service"
)

// Command es un pedido del control plane. El ID del envelope es el ID del
// comando: el agent lo devuelve en la respuesta y lo usa para no ejecutar dos
// veces el mismo pedido si llega repetido.
//
// Lleva los parámetros de su operación en su propio campo, tipado: no hay un
// mapa de parámetros sueltos que cada operación interprete a su manera.
type Command struct {
	Operation Operation       `json:"operation"`
	GetLogs   *GetLogsParams  `json:"get_logs,omitempty"`
	Deploy    *DeployParams   `json:"deploy,omitempty"`
	Rollback  *RollbackParams `json:"rollback,omitempty"`
}

// Type implementa Message.
func (Command) Type() MessageType { return TypeCommand }

// Validate implementa Message. Un comando trae los parámetros de su operación
// y ningún otro: parámetros de más son un error, no algo que se ignora.
func (c Command) Validate() error {
	params := 0
	for _, present := range []bool{c.GetLogs != nil, c.Deploy != nil, c.Rollback != nil} {
		if present {
			params++
		}
	}
	if params > 1 {
		return errors.New("el comando trae parámetros de más de una operación")
	}

	switch c.Operation {
	case OpGetLogs:
		if c.GetLogs == nil {
			return errors.New("get_logs sin parámetros")
		}
		return c.GetLogs.Validate()
	case OpDeploy:
		if c.Deploy == nil {
			return errors.New("deploy sin parámetros")
		}
		return c.Deploy.Validate()
	case OpRollback:
		if c.Rollback == nil {
			return errors.New("rollback sin parámetros")
		}
		return c.Rollback.Validate()
	case "":
		return errors.New("falta la operación")
	default:
		return fmt.Errorf("%w: %q", ErrUnknownOperation, c.Operation)
	}
}

// ErrUnknownOperation es una operación que no está en la lista cerrada.
var ErrUnknownOperation = errors.New("operación desconocida")

// GetLogsParams dice qué log traer. Target es el ID completo del contenedor o
// el nombre de la unidad, con los mismos caracteres que acepta la foto de
// runtime: el agent además verifica que exista en su última foto.
type GetLogsParams struct {
	Kind   LogKind `json:"kind"`
	Target string  `json:"target"`
	Lines  int     `json:"lines"`
}

// Validate revisa el objetivo según su tipo y la cantidad de renglones.
func (p GetLogsParams) Validate() error {
	switch p.Kind {
	case LogContainer:
		if !isContainerID(p.Target) {
			return fmt.Errorf("contenedor inválido %q", p.Target)
		}
	case LogService:
		if !validName(p.Target, MaxRuntimeNameLen, isUnitRune) || !strings.HasSuffix(p.Target, ".service") {
			return fmt.Errorf("servicio inválido %q", p.Target)
		}
	default:
		return fmt.Errorf("tipo de log desconocido %q", p.Kind)
	}
	if p.Lines < 1 || p.Lines > MaxLogLines {
		return fmt.Errorf("los renglones van de 1 a %d, llegó %d", MaxLogLines, p.Lines)
	}
	return nil
}

// CommandStatus es cómo terminó un comando.
type CommandStatus string

// Resultados de un comando.
const (
	// CommandOK es que se ejecutó y trae su resultado.
	CommandOK CommandStatus = "ok"
	// CommandRejected es que el agent no lo ejecutó: operación que no conoce,
	// objetivo que no existe, demasiados comandos a la vez.
	CommandRejected CommandStatus = "rejected"
	// CommandFailed es que se intentó y falló.
	CommandFailed CommandStatus = "failed"
)

// CommandErrorCode dice por qué un comando no terminó bien. Como los códigos
// de error del canal, es una lista cerrada: quien lo recibe decide mirando el
// código, no el texto.
type CommandErrorCode string

// Códigos de un comando que no terminó bien.
const (
	CommandUnknownOperation CommandErrorCode = "unknown_operation"
	CommandInvalidParams    CommandErrorCode = "invalid_params"
	CommandUnknownTarget    CommandErrorCode = "unknown_target"
	CommandBusy             CommandErrorCode = "busy"
	CommandUnavailable      CommandErrorCode = "unavailable"
	CommandTimeout          CommandErrorCode = "timeout"
	CommandInternal         CommandErrorCode = "internal"
	// CommandDisabled es una operación de escritura que una persona no
	// prendió en ese servidor (ADR-015).
	CommandDisabled CommandErrorCode = "disabled"
)

var knownCommandCodes = map[CommandErrorCode]bool{
	CommandUnknownOperation: true, CommandInvalidParams: true, CommandUnknownTarget: true,
	CommandBusy: true, CommandUnavailable: true, CommandTimeout: true, CommandInternal: true,
	CommandDisabled: true,
}

// CommandAck es la respuesta del agent a un comando: el acuse y el resultado
// juntos, porque get_logs termina enseguida. Un deploy o un rollback se
// contestan apenas se aceptan (o se rechazan), sin resultado: el avance y el
// final llegan aparte, en deploy_event, con el ID del comando.
type CommandAck struct {
	CommandID uuid.UUID        `json:"command_id"`
	Status    CommandStatus    `json:"status"`
	Code      CommandErrorCode `json:"code,omitempty"`
	Message   string           `json:"message,omitempty"`
	Logs      *LogsResult      `json:"logs,omitempty"`
}

// Type implementa Message.
func (CommandAck) Type() MessageType { return TypeCommandAck }

// Validate implementa Message.
func (a CommandAck) Validate() error {
	if a.CommandID == uuid.Nil {
		return errors.New("falta el ID del comando")
	}
	switch a.Status {
	case CommandOK:
		if a.Code != "" {
			return errors.New("un comando que terminó bien no lleva código de error")
		}
	case CommandRejected, CommandFailed:
		if !knownCommandCodes[a.Code] {
			return fmt.Errorf("código de error desconocido %q", a.Code)
		}
		if a.Logs != nil {
			return errors.New("un comando que no terminó bien no trae resultado")
		}
	default:
		return fmt.Errorf("estado desconocido %q", a.Status)
	}
	if len(a.Message) > maxCommandMessageLen || !isPrintable(a.Message) {
		return errors.New("el texto del resultado es demasiado largo o tiene caracteres de control")
	}
	if a.Logs != nil {
		return a.Logs.Validate()
	}
	return nil
}

// LogsResult son los renglones de log pedidos, del más viejo al más nuevo, con
// los secretos ya enmascarados por el agent (ADR-011).
type LogsResult struct {
	Lines []LogLine `json:"lines"`
	// Truncated dice que se dejaron afuera los renglones más viejos para que
	// la respuesta entre en un mensaje.
	Truncated bool `json:"truncated,omitempty"`
}

// Validate revisa la cantidad de renglones y cada uno.
func (r LogsResult) Validate() error {
	if len(r.Lines) > MaxLogLines {
		return fmt.Errorf("trae %d renglones y el máximo es %d", len(r.Lines), MaxLogLines)
	}
	for i, line := range r.Lines {
		if err := line.Validate(); err != nil {
			return fmt.Errorf("renglón %d: %w", i, err)
		}
	}
	return nil
}

// LogLine es un renglón de log. Stream es stdout o stderr en un contenedor; en
// un servicio, stderr marca los mensajes de prioridad de error o más grave.
type LogLine struct {
	TS     time.Time `json:"ts,omitzero"`
	Stream string    `json:"stream,omitempty"`
	Text   string    `json:"text"`
}

// Validate revisa el largo y que el texto no traiga caracteres de control: un
// log puede traer secuencias de escape de terminal que no tienen por qué
// llegar al navegador.
func (l LogLine) Validate() error {
	switch l.Stream {
	case "", "stdout", "stderr":
	default:
		return fmt.Errorf("stream desconocido %q", l.Stream)
	}
	if len(l.Text) > MaxLogLineLen || !utf8.ValidString(l.Text) || !isPrintable(l.Text) {
		return errors.New("renglón demasiado largo o con caracteres de control")
	}
	return nil
}
