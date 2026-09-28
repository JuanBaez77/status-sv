package api

import (
	"context"
	"errors"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/projects"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// Límite de lecturas de proyectos por canal. El agent manda una al conectar,
// cuando cambia un archivo y cada 5 minutos: más que esto es un error.
const (
	projectsPerSecond = rate.Limit(0.2)
	projectsBurst     = 5
)

// handleProjectsState guarda los proyectos declarados que leyó el agent
// (ADR-012) y le contesta, igual que con las fotos de runtime:
//
//   - projects_state_ack: guardada, o más vieja que la guardada.
//   - invalid_message: no se va a aceptar nunca; la descarta.
//   - rate_limited o internal: la vuelve a mandar más tarde.
func (s *Server) handleProjectsState(ctx context.Context, conn *websocket.Conn, server store.Server, limiter *rate.Limiter, envelope proto.Envelope) error {
	if !limiter.Allow() {
		s.log.WarnContext(ctx, "proyectos rechazados por frecuencia", "servidor", server.Name)
		s.replyError(ctx, conn, envelope.ID, proto.CodeRateLimited, "demasiadas lecturas de proyectos seguidas")
		return nil
	}

	state, err := proto.Decode[proto.ProjectsState](envelope)
	if err != nil {
		s.log.WarnContext(ctx, "proyectos inválidos", "servidor", server.Name, "error", err)
		s.replyError(ctx, conn, envelope.ID, proto.CodeInvalidMessage, err.Error())
		return nil
	}

	ingestCtx, cancel := context.WithTimeout(ctx, ingestTimeout)
	defer cancel()

	applied, err := s.projects.Ingest(ingestCtx, server.ID, state)
	switch {
	case errors.Is(err, projects.ErrInvalidState):
		s.log.WarnContext(ctx, "proyectos rechazados", "servidor", server.Name, "error", err)
		s.replyError(ctx, conn, envelope.ID, proto.CodeInvalidMessage, err.Error())
		return nil
	case err != nil:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.log.ErrorContext(ctx, "no se pudieron guardar los proyectos", "servidor", server.Name, "error", err)
		s.replyError(ctx, conn, envelope.ID, proto.CodeInternal, "no se pudieron guardar los proyectos; reintentar más tarde")
		return nil
	}

	s.log.InfoContext(ctx, "proyectos declarados",
		"servidor", server.Name,
		"aplicada", applied,
		"fuente", state.Source.Status,
		"validos", len(state.Projects),
		"invalidos", len(state.Invalid),
	)
	return s.writeMessage(ctx, conn, proto.ProjectsStateAck{TakenAt: state.TakenAt})
}
