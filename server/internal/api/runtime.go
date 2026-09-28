package api

import (
	"context"
	"errors"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/runstate"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// Límite de fotos de runtime por canal. Un agent manda una cuando algo cambia
// (con un respiro para juntar ráfagas) y cada tanto con el consumo al día; un
// contenedor que se reinicia en bucle puede mandar varias seguidas. Pasado el
// límite contesta rate_limited y el agent manda la foto más nueva más tarde:
// una foto entera reemplaza a las que se perdieron.
const (
	runtimePerSecond = rate.Limit(1)
	runtimeBurst     = 10
)

// channelLimits son los límites de frecuencia de un canal de agent.
type channelLimits struct {
	metrics  *rate.Limiter
	runtime  *rate.Limiter
	projects *rate.Limiter
}

// handleRuntimeState guarda una foto de runtime y le contesta al agent:
//
//   - runtime_state_ack: guardada, o más vieja que la guardada; deja de reenviarla.
//   - invalid_message: no se va a aceptar nunca; la descarta.
//   - rate_limited o internal: la vuelve a mandar más tarde.
func (s *Server) handleRuntimeState(ctx context.Context, conn *websocket.Conn, server store.Server, limiter *rate.Limiter, envelope proto.Envelope) error {
	if !limiter.Allow() {
		s.log.WarnContext(ctx, "foto de runtime rechazada por frecuencia", "servidor", server.Name)
		s.replyError(ctx, conn, envelope.ID, proto.CodeRateLimited, "demasiadas fotos de runtime seguidas")
		return nil
	}

	state, err := proto.Decode[proto.RuntimeState](envelope)
	if err != nil {
		s.log.WarnContext(ctx, "foto de runtime inválida", "servidor", server.Name, "error", err)
		s.replyError(ctx, conn, envelope.ID, proto.CodeInvalidMessage, err.Error())
		return nil
	}

	ingestCtx, cancel := context.WithTimeout(ctx, ingestTimeout)
	defer cancel()

	applied, err := s.runtime.Ingest(ingestCtx, server.ID, state)
	switch {
	case errors.Is(err, runstate.ErrInvalidState):
		s.log.WarnContext(ctx, "foto de runtime rechazada", "servidor", server.Name, "error", err)
		s.replyError(ctx, conn, envelope.ID, proto.CodeInvalidMessage, err.Error())
		return nil
	case err != nil:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.log.ErrorContext(ctx, "no se pudo guardar una foto de runtime", "servidor", server.Name, "error", err)
		s.replyError(ctx, conn, envelope.ID, proto.CodeInternal, "no se pudo guardar la foto; reintentar más tarde")
		return nil
	}

	// Los paneles que miran este servidor la ven al instante.
	if applied {
		s.publishRuntime(ctx, server.ID)
	}

	s.log.DebugContext(ctx, "foto de runtime",
		"servidor", server.Name,
		"aplicada", applied,
		"contenedores", len(state.Containers),
		"servicios", len(state.Services),
		"procesos", len(state.Processes),
	)
	return s.writeMessage(ctx, conn, proto.RuntimeStateAck{TakenAt: state.TakenAt})
}
