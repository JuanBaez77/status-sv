package ws

import (
	"context"

	"github.com/coder/websocket"

	"github.com/JuanBaez77/status-sv/agent/internal/projects"
	"github.com/JuanBaez77/status-sv/agent/internal/runtimestate"
	"github.com/JuanBaez77/status-sv/proto"
)

// SetRuntime le da al cliente la bandeja de las fotos de runtime. Se llama
// antes de Run.
func (c *Client) SetRuntime(outbox *runtimestate.Outbox) {
	c.runtime = outbox
}

// SetProjects le da al cliente la bandeja de los proyectos declarados. Se llama
// antes de Run.
func (c *Client) SetProjects(outbox *projects.Outbox) {
	c.projects = outbox
}

func (c *Client) runtimeLoop(ctx context.Context, conn *websocket.Conn) error {
	return snapshotLoop(ctx, c, conn, c.runtime, "la foto de runtime")
}

func (c *Client) projectsLoop(ctx context.Context, conn *websocket.Conn) error {
	return snapshotLoop(ctx, c, conn, c.projects, "los proyectos declarados")
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

// handleProjectsAck da por entregados los proyectos que confirmó el control
// plane.
func (c *Client) handleProjectsAck(ctx context.Context, envelope proto.Envelope) {
	ack, err := proto.Decode[proto.ProjectsStateAck](envelope)
	if err != nil {
		c.log.WarnContext(ctx, "acuse de proyectos inválido", "error", err)
		return
	}
	if c.projects != nil {
		c.projects.Acked(ack.TakenAt)
	}
}
