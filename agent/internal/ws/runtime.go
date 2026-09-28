package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coder/websocket"

	"github.com/JuanBaez77/status-sv/agent/internal/runtimestate"
	"github.com/JuanBaez77/status-sv/proto"
)

// SetRuntime le da al cliente la bandeja de las fotos de runtime. Se llama
// antes de Run.
func (c *Client) SetRuntime(outbox *runtimestate.Outbox) {
	c.runtime = outbox
}

// runtimeLoop manda las fotos de runtime a medida que aparecen en la bandeja,
// una en vuelo por vez. Se despierta cuando hay una foto nueva o llega un
// acuse, y cada tanto para reenviar una que se quedó sin respuesta.
func (c *Client) runtimeLoop(ctx context.Context, conn *websocket.Conn) error {
	retry := time.NewTicker(runtimestate.AckTimeout / 3)
	defer retry.Stop()
	for {
		if err := c.sendRuntime(ctx, conn); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.runtime.Wake():
		case <-retry.C:
		}
	}
}

func (c *Client) sendRuntime(ctx context.Context, conn *websocket.Conn) error {
	state, ok := c.runtime.Next(time.Now())
	if !ok {
		return nil
	}

	envelope, err := proto.Encode(state)
	if err != nil {
		// No pasa: el ensamblador valida cada unidad. Si pasara, la foto
		// trabaría la bandeja, así que se descarta y sale la próxima.
		c.log.ErrorContext(ctx, "se descarta una foto de runtime que no se puede mandar", "error", err)
		c.runtime.Sent(envelope.ID, state.TakenAt, time.Now())
		c.runtime.Rejected(envelope.ID, false)
		return nil
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("serializar la foto de runtime: %w", err)
	}

	// Se registra antes de escribir: el acuse puede llegar antes de que Write
	// termine de volver.
	c.runtime.Sent(envelope.ID, state.TakenAt, time.Now())

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, raw); err != nil {
		return fmt.Errorf("enviar la foto de runtime: %w", err)
	}
	c.log.DebugContext(ctx, "foto de runtime enviada", "bytes", len(raw))
	return nil
}

// handleRuntimeAck da por entregada la foto que confirmó el control plane.
func (c *Client) handleRuntimeAck(ctx context.Context, envelope proto.Envelope) {
	ack, err := proto.Decode[proto.RuntimeStateAck](envelope)
	if err != nil {
		c.log.WarnContext(ctx, "acuse de foto de runtime inválido", "error", err)
		return
	}
	if c.runtime != nil {
		c.runtime.Acked(ack.TakenAt)
	}
}
