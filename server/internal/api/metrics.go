package api

import (
	"context"
	"errors"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/metrics"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// Límite de lotes de métricas por canal. Un agent normal manda uno cada 10
// segundos, y al vaciar su buffer después de un corte, unos pocos seguidos. El
// límite está para que un agent roto o comprometido no pueda inundar la base.
const (
	metricsPerSecond = rate.Limit(1)
	metricsBurst     = 30
)

// ingestTimeout acota lo que puede tardar guardar un lote. El agent espera el
// acuse 30 segundos antes de reenviar.
const ingestTimeout = 10 * time.Second

// handleMetrics guarda un lote y le contesta al agent. Cada respuesta le dice
// qué hacer con esas muestras (ver proto/README.md):
//
//   - metrics_ack: están guardadas; las suelta de su buffer.
//   - invalid_message: no se van a aceptar nunca; las descarta.
//   - rate_limited o internal: las vuelve a mandar más tarde.
func (s *Server) handleMetrics(ctx context.Context, conn *websocket.Conn, server store.Server, limiter *rate.Limiter, envelope proto.Envelope) error {
	if !limiter.Allow() {
		s.log.WarnContext(ctx, "lote de métricas rechazado por frecuencia", "servidor", server.Name)
		s.replyError(ctx, conn, envelope.ID, proto.CodeRateLimited, "demasiados lotes de métricas seguidos")
		return nil
	}

	batch, err := proto.Decode[proto.Metrics](envelope)
	if err != nil {
		s.log.WarnContext(ctx, "lote de métricas inválido", "servidor", server.Name, "error", err)
		s.replyError(ctx, conn, envelope.ID, proto.CodeInvalidMessage, err.Error())
		return nil
	}

	ingestCtx, cancel := context.WithTimeout(ctx, ingestTimeout)
	defer cancel()

	result, err := s.ingester.Ingest(ingestCtx, server.ID, batch.Samples)
	switch {
	case errors.Is(err, metrics.ErrInvalidBatch):
		s.log.WarnContext(ctx, "lote de métricas rechazado", "servidor", server.Name, "error", err)
		s.replyError(ctx, conn, envelope.ID, proto.CodeInvalidMessage, err.Error())
		return nil
	case err != nil:
		// Si el canal se cortó mientras se guardaba, no hay a quién contestarle.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.log.ErrorContext(ctx, "no se pudo guardar un lote de métricas", "servidor", server.Name, "error", err)
		s.replyError(ctx, conn, envelope.ID, proto.CodeInternal, "no se pudo guardar el lote; reintentar más tarde")
		return nil
	}

	s.log.DebugContext(ctx, "lote de métricas guardado",
		"servidor", server.Name,
		"muestras", result.Accepted,
		"nuevas", result.New,
	)
	return s.writeMessage(ctx, conn, proto.MetricsAck{Accepted: result.Accepted, LastTS: result.LastTS})
}
