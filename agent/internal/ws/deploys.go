package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coder/websocket"

	"github.com/JuanBaez77/status-sv/agent/internal/deploy"
	"github.com/JuanBaez77/status-sv/proto"
)

// SetDeploys le da al cliente la bandeja de las fotos de los deploys. Se llama
// antes de Run.
func (c *Client) SetDeploys(outbox *deploy.Outbox) {
	c.deploys = outbox
}

// deploysLoop manda las fotos de los deploys, de a una en vuelo, como las
// otras fotos. La bandeja guarda la última de cada deploy hasta su acuse, así
// que lo que no llegó en este canal sale en el siguiente.
func (c *Client) deploysLoop(ctx context.Context, conn *websocket.Conn) error {
	retry := time.NewTicker(deploy.AckTimeout / 3)
	defer retry.Stop()
	for {
		if err := c.sendDeployEvent(ctx, conn); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.deploys.Wake():
		case <-retry.C:
		}
	}
}

func (c *Client) sendDeployEvent(ctx context.Context, conn *websocket.Conn) error {
	ev, ok := c.deploys.Next(time.Now())
	if !ok {
		return nil
	}
	envelope, err := proto.Encode(ev)
	if err != nil {
		// No pasa: el motor valida cada foto. Si pasara, trabaría la bandeja,
		// así que se descarta.
		c.log.ErrorContext(ctx, "se descarta una foto de deploy que no se puede mandar", "deploy", ev.DeployID, "error", err)
		c.deploys.Sent(envelope.ID, ev, time.Now())
		c.deploys.Rejected(envelope.ID, false)
		return nil
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("serializar la foto del deploy: %w", err)
	}

	// Se registra antes de escribir: el acuse puede llegar antes de que Write
	// termine de volver.
	c.deploys.Sent(envelope.ID, ev, time.Now())

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, raw); err != nil {
		return fmt.Errorf("enviar la foto del deploy: %w", err)
	}
	c.log.DebugContext(ctx, "foto de deploy enviada", "deploy", ev.DeployID, "seq", ev.Seq, "estado", ev.Status)
	return nil
}

// handleDeployAck da por entregada la foto que confirmó el control plane.
func (c *Client) handleDeployAck(ctx context.Context, envelope proto.Envelope) {
	ack, err := proto.Decode[proto.DeployEventAck](envelope)
	if err != nil {
		c.log.WarnContext(ctx, "acuse de foto de deploy inválido", "error", err)
		return
	}
	if c.deploys != nil {
		c.deploys.Acked(ack.DeployID, ack.Seq)
	}
}
