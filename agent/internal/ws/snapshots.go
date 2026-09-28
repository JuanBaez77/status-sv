package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coder/websocket"

	"github.com/JuanBaez77/status-sv/agent/internal/outbox"
	"github.com/JuanBaez77/status-sv/proto"
)

// snapshotLoop manda las fotos de una bandeja a medida que aparecen, una en
// vuelo por vez. Se despierta cuando hay una foto nueva o llega un acuse, y
// cada tanto para reenviar una que se quedó sin respuesta. Lo usan el estado
// de runtime y los proyectos declarados.
func snapshotLoop[T proto.Message](ctx context.Context, c *Client, conn *websocket.Conn, box *outbox.Box[T], what string) error {
	retry := time.NewTicker(outbox.AckTimeout / 3)
	defer retry.Stop()
	for {
		if err := sendSnapshot(ctx, c, conn, box, what); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-box.Wake():
		case <-retry.C:
		}
	}
}

func sendSnapshot[T proto.Message](ctx context.Context, c *Client, conn *websocket.Conn, box *outbox.Box[T], what string) error {
	snapshot, ok := box.Next(time.Now())
	if !ok {
		return nil
	}
	takenAt := box.Stamp(snapshot)

	envelope, err := proto.Encode(snapshot)
	if err != nil {
		// No pasa: quien arma la foto la valida. Si pasara, la foto trabaría
		// la bandeja, así que se descarta y sale la próxima.
		c.log.ErrorContext(ctx, "se descarta una foto que no se puede mandar", "tipo", what, "error", err)
		box.Sent(envelope.ID, takenAt, time.Now())
		box.Rejected(envelope.ID, false)
		return nil
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("serializar %s: %w", what, err)
	}

	// Se registra antes de escribir: el acuse puede llegar antes de que Write
	// termine de volver.
	box.Sent(envelope.ID, takenAt, time.Now())

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, raw); err != nil {
		return fmt.Errorf("enviar %s: %w", what, err)
	}
	c.log.DebugContext(ctx, "foto enviada", "tipo", what, "bytes", len(raw))
	return nil
}
