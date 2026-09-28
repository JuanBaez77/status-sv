package ws

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/agent/internal/commands"
	"github.com/JuanBaez77/status-sv/proto"
)

// commandHandler ejecuta un comando y devuelve la respuesta. Lo cumple
// commands.Executor.
type commandHandler interface {
	Handle(ctx context.Context, id uuid.UUID, cmd proto.Command) proto.CommandAck
}

// commandTimeout acota lo que puede tardar un comando de punta a punta.
const commandTimeout = 20 * time.Second

// SetCommands le da al cliente con qué ejecutar los comandos del control
// plane. Se llama antes de Run.
func (c *Client) SetCommands(h commandHandler) {
	c.commands = h
}

// handleCommand ejecuta un comando y manda la respuesta por el mismo canal.
// Un comando que no se puede leer, o que llega sin ejecutor, se rechaza: el
// agent nunca adivina qué se le quiso pedir.
func (c *Client) handleCommand(ctx context.Context, conn *websocket.Conn, envelope proto.Envelope) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	cmd, err := proto.Decode[proto.Command](envelope)
	var ack proto.CommandAck
	switch {
	case err != nil:
		c.log.WarnContext(ctx, "comando rechazado", "comando", envelope.ID, "error", err)
		ack = commands.Rejected(envelope.ID, err)
	case c.commands == nil:
		ack = commands.Rejected(envelope.ID, proto.ErrUnknownOperation)
	default:
		c.log.InfoContext(ctx, "comando recibido", "comando", envelope.ID, "operacion", cmd.Operation)
		ack = c.commands.Handle(ctx, envelope.ID, cmd)
	}

	message, err := proto.Encode(ack)
	if err != nil {
		c.log.ErrorContext(ctx, "no se pudo armar la respuesta a un comando", "comando", envelope.ID, "error", err)
		return
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return
	}
	writeCtx, cancelWrite := context.WithTimeout(ctx, writeTimeout)
	defer cancelWrite()
	if err := conn.Write(writeCtx, websocket.MessageText, raw); err != nil && !errors.Is(err, context.Canceled) {
		c.log.WarnContext(ctx, "no se pudo mandar la respuesta a un comando", "comando", envelope.ID, "error", err)
		return
	}
	c.log.InfoContext(ctx, "comando respondido", "comando", envelope.ID, "estado", ack.Status, "codigo", ack.Code)
}
