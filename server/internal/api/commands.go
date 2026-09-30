package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/audit"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// commandWait es cuánto se espera la respuesta de un agent. El agent se da 20
// segundos para ejecutar un comando; esto le suma el viaje.
const commandWait = 25 * time.Second

// maxLogsBody acota el pedido de logs: son tres campos.
const maxLogsBody = 4 << 10

// Errores al mandar un comando.
var (
	errAgentOffline = errors.New("el agent de ese servidor no está conectado")
	errNoAnswer     = errors.New("el agent no contestó a tiempo")
)

// pendingCommands son los comandos mandados que esperan respuesta, por ID.
//
// Cada uno recuerda a qué servidor se mandó: una respuesta solo vale si llega
// por el canal de ese mismo agent. Un agent no puede contestar por otro.
type pendingCommands struct {
	mu      sync.Mutex
	waiting map[uuid.UUID]pendingCommand
}

type pendingCommand struct {
	serverID uuid.UUID
	answer   chan proto.CommandAck
}

func newPendingCommands() *pendingCommands {
	return &pendingCommands{waiting: make(map[uuid.UUID]pendingCommand)}
}

func (p *pendingCommands) register(id, serverID uuid.UUID) chan proto.CommandAck {
	answer := make(chan proto.CommandAck, 1)
	p.mu.Lock()
	p.waiting[id] = pendingCommand{serverID: serverID, answer: answer}
	p.mu.Unlock()
	return answer
}

func (p *pendingCommands) forget(id uuid.UUID) {
	p.mu.Lock()
	delete(p.waiting, id)
	p.mu.Unlock()
}

// resolve entrega una respuesta a quien la espera. Devuelve false si nadie la
// esperaba o si llegó por el canal de otro servidor.
func (p *pendingCommands) resolve(serverID uuid.UUID, ack proto.CommandAck) bool {
	p.mu.Lock()
	pending, ok := p.waiting[ack.CommandID]
	if ok && pending.serverID == serverID {
		delete(p.waiting, ack.CommandID)
	}
	p.mu.Unlock()
	if !ok || pending.serverID != serverID {
		return false
	}
	pending.answer <- ack
	return true
}

// sendCommand manda un comando al agent de un servidor y espera su respuesta.
func (s *Server) sendCommand(ctx context.Context, serverID uuid.UUID, cmd proto.Command) (proto.CommandAck, error) {
	return s.sendCommandWithID(ctx, serverID, uuid.Nil, cmd)
}

// sendCommandWithID es sendCommand con el ID del comando elegido por quien
// llama: el de un deploy es el ID del deploy, que el agent devuelve en cada
// foto. Con uuid.Nil se genera uno.
func (s *Server) sendCommandWithID(ctx context.Context, serverID, id uuid.UUID, cmd proto.Command) (proto.CommandAck, error) {
	conn := s.hub.get(serverID)
	if conn == nil {
		return proto.CommandAck{}, errAgentOffline
	}

	envelope, err := proto.Encode(cmd)
	if err != nil {
		return proto.CommandAck{}, fmt.Errorf("armar el comando: %w", err)
	}
	if id != uuid.Nil {
		envelope.ID = id
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return proto.CommandAck{}, fmt.Errorf("serializar el comando: %w", err)
	}

	answer := s.pending.register(envelope.ID, serverID)
	defer s.pending.forget(envelope.ID)

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	err = conn.Write(writeCtx, websocket.MessageText, raw)
	cancel()
	if err != nil {
		return proto.CommandAck{}, fmt.Errorf("%w: %w", errAgentOffline, err)
	}

	waitCtx, cancelWait := context.WithTimeout(ctx, commandWait)
	defer cancelWait()
	select {
	case ack := <-answer:
		return ack, nil
	case <-waitCtx.Done():
		if ctx.Err() != nil {
			return proto.CommandAck{}, ctx.Err()
		}
		return proto.CommandAck{}, errNoAnswer
	}
}

// handleCommandAck entrega al que espera la respuesta de un agent a un
// comando. Una respuesta que nadie espera (llegó tarde, o por el canal de otro
// servidor) se descarta.
func (s *Server) handleCommandAck(ctx context.Context, server store.Server, envelope proto.Envelope) {
	ack, err := proto.Decode[proto.CommandAck](envelope)
	if err != nil {
		s.log.WarnContext(ctx, "respuesta a un comando inválida", "servidor", server.Name, "error", err)
		return
	}
	if !s.pending.resolve(server.ID, ack) {
		s.log.DebugContext(ctx, "respuesta a un comando que nadie espera", "servidor", server.Name, "comando", ack.CommandID)
	}
}

// logsRequest es el pedido de logs del panel.
type logsRequest struct {
	Kind   proto.LogKind `json:"kind"`
	Target string        `json:"target"`
	Lines  int           `json:"lines"`
}

// logsResponse es lo que vuelve al panel: la respuesta del agent, tal cual.
// Un comando que el agent rechazó o que falló no es un error de HTTP: el panel
// muestra el motivo.
type logsResponse struct {
	Status    proto.CommandStatus    `json:"status"`
	Code      proto.CommandErrorCode `json:"code,omitempty"`
	Message   string                 `json:"message,omitempty"`
	Lines     []proto.LogLine        `json:"lines"`
	Truncated bool                   `json:"truncated"`
}

// handleServerLogs le pide al agent los últimos renglones de log de un
// contenedor o servicio: POST /v1/servers/{id}/logs.
func (s *Server) handleServerLogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)

	serverID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el ID del servidor no es un UUID"})
		return
	}

	var req logsRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLogsBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el pedido no es un JSON válido"})
		return
	}
	params := proto.GetLogsParams(req)
	if err := params.Validate(); err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: err.Error()})
		return
	}

	server, err := s.queries.GetServerByID(ctx, serverID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(ctx, w, http.StatusNotFound, errorResponse{Error: "not_found", Message: "no existe ese servidor"})
			return
		}
		s.log.ErrorContext(ctx, "no se pudo buscar el servidor", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}

	ack, err := s.sendCommand(ctx, serverID, proto.Command{Operation: proto.OpGetLogs, GetLogs: &params})
	// Quién pidió qué queda en el registro y en la auditoría (ADR-017).
	result := string(ack.Status)
	if err != nil {
		result = "error"
	}
	s.record(ctx, audit.Entry{
		Actor: SessionFrom(ctx).Username, ActorType: audit.ActorUser, Action: audit.ActionLogsRequest,
		TargetType: "server", TargetID: server.ID.String(), Result: result, IP: s.clientIP(r),
		Detail: map[string]any{"servidor": server.Name, "tipo": string(params.Kind), "objetivo": params.Target, "renglones": params.Lines},
	})
	s.log.InfoContext(ctx, "logs pedidos",
		"usuario", SessionFrom(ctx).Username,
		"servidor", server.Name,
		"tipo", params.Kind,
		"objetivo", params.Target,
		"renglones", params.Lines,
		"estado", ack.Status,
		"error", err,
	)
	switch {
	case errors.Is(err, errAgentOffline):
		writeJSON(ctx, w, http.StatusConflict, errorResponse{Error: "agent_offline", Message: "el agent de ese servidor no está conectado"})
		return
	case errors.Is(err, errNoAnswer):
		writeJSON(ctx, w, http.StatusGatewayTimeout, errorResponse{Error: "no_answer", Message: "el agent no contestó a tiempo"})
		return
	case err != nil:
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}

	out := logsResponse{Status: ack.Status, Code: ack.Code, Message: ack.Message, Lines: []proto.LogLine{}}
	if ack.Logs != nil {
		out.Lines = ack.Logs.Lines
		out.Truncated = ack.Logs.Truncated
	}
	writeJSON(ctx, w, http.StatusOK, out)
}
