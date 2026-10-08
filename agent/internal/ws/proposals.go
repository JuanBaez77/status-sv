package ws

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/coder/websocket"

	"github.com/JuanBaez77/status-sv/proto"
)

// proposalHandler guarda o retira una propuesta de un proyecto nuevo
// (ADR-022). Lo cumple proposals.Store.
type proposalHandler interface {
	Handle(p proto.ProjectProposal) proto.ProjectProposalAck
}

// SetProposals le da al cliente dónde guardar las propuestas que manda el
// control plane, y a quién avisarle cuando cambian (la lectura de proyectos,
// que las informa). Se llama antes de Run. Sin esto, las propuestas se
// rechazan.
func (c *Client) SetProposals(h proposalHandler, changed func()) {
	c.proposals = h
	c.proposalsChanged = changed
}

// handleProposal atiende una propuesta y manda el acuse por el mismo canal. No
// ejecuta nada: la guarda hasta que una persona la acepta en el servidor.
func (c *Client) handleProposal(ctx context.Context, conn *websocket.Conn, envelope proto.Envelope) {
	p, err := proto.Decode[proto.ProjectProposal](envelope)
	if err != nil {
		// Sin poder leerla no se sabe a qué propuesta contestar: el control
		// plane la reenvía al reconectar.
		c.log.WarnContext(ctx, "propuesta de proyecto ilegible", "mensaje", envelope.ID, "error", err)
		return
	}
	ack := proto.ProjectProposalAck{
		ProposalID: p.ProposalID,
		Name:       p.Name,
		Result:     proto.ProposalRejected,
		Error:      "este agent no guarda propuestas",
	}
	if c.proposals != nil {
		ack = c.proposals.Handle(p)
	}
	attrs := []any{"propuesta", p.ProposalID, "proyecto", p.Name, "accion", p.Action, "resultado", ack.Result}
	if ack.Result == proto.ProposalRejected {
		c.log.WarnContext(ctx, "propuesta de proyecto rechazada", append(attrs, "error", ack.Error)...)
	} else {
		c.log.InfoContext(ctx, "propuesta de proyecto", attrs...)
		if c.proposalsChanged != nil {
			c.proposalsChanged()
		}
	}

	message, err := proto.Encode(ack)
	if err != nil {
		c.log.ErrorContext(ctx, "no se pudo armar el acuse de una propuesta", "propuesta", p.ProposalID, "error", err)
		return
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, raw); err != nil && !errors.Is(err, context.Canceled) {
		c.log.WarnContext(ctx, "no se pudo mandar el acuse de una propuesta", "propuesta", p.ProposalID, "error", err)
	}
}
