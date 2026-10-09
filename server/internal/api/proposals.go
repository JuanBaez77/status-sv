package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/audit"
	"github.com/JuanBaez77/status-sv/server/internal/proposals"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// El alta de proyectos (ADR-022). El panel propone un proyecto nuevo con
// POST /v1/servers/{id}/proposals; el control plane la guarda y se la manda
// al agent del servidor en project_proposal. Nada cambia en el servidor hasta
// que una persona la acepta ahí con `statussv-agent project accept`: el
// control plane se entera cuando el agent informa el proyecto declarado.

// Límites de las propuestas.
const (
	// maxProposalBody acota el pedido: una declaración real pesa menos de
	// 2 KiB.
	maxProposalBody = 64 << 10
	// proposalRequestsPerMinute acota las propuestas por IP.
	proposalRequestsPerMinute = 20
	proposalRequestsBurst     = 10
)

// panelProposals es el aviso del WebSocket del panel de que cambiaron las
// propuestas de un servidor.
const panelProposals = "proposals"

type panelProposalsMessage struct {
	Type     string    `json:"type"`
	ServerID uuid.UUID `json:"server_id"`
}

// proposalSummary es una propuesta como la muestra la lista.
type proposalSummary struct {
	ID          uuid.UUID  `json:"id"`
	ServerID    uuid.UUID  `json:"server_id"`
	Name        string     `json:"name"`
	Runtime     string     `json:"runtime"`
	Environment string     `json:"environment"`
	Status      string     `json:"status"`
	Message     string     `json:"message,omitempty"`
	CreatedBy   string     `json:"created_by"`
	ActorType   string     `json:"actor_type"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
	ClosedAt    *time.Time `json:"closed_at,omitempty"`
	// Command es lo que se corre en el servidor para aceptarla.
	Command string `json:"command"`
}

// proposalDetail es una propuesta con lo que se va a escribir en el servidor.
type proposalDetail struct {
	proposalSummary
	Preview proposals.Preview `json:"preview"`
}

type proposalListResponse struct {
	Proposals []proposalSummary `json:"proposals"`
}

func summaryOf(p store.ProjectProposal) proposalSummary {
	return proposalSummary{
		ID: p.ID, ServerID: p.ServerID, Name: p.Name, Runtime: p.Runtime, Environment: p.Environment,
		Status: p.Status, Message: p.Message, CreatedBy: p.CreatedByName, ActorType: p.ActorType,
		CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt, DeliveredAt: p.DeliveredAt, ClosedAt: p.ClosedAt,
		Command: proposals.AcceptCommand(p.Name),
	}
}

// decodeProposal lee el pedido de una propuesta.
func decodeProposal(w http.ResponseWriter, r *http.Request) (proposals.Request, bool) {
	var req proposals.Request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxProposalBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(r.Context(), w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el pedido no es un JSON válido"})
		return req, false
	}
	return req, true
}

// handlePreviewProposal arma la declaración y lo que generan las plantillas,
// sin guardar nada: POST /v1/servers/{id}/proposals/preview.
func (s *Server) handlePreviewProposal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)
	req, ok := decodeProposal(w, r)
	if !ok {
		return
	}
	preview, err := proposals.MakePreview(req)
	if err != nil {
		s.writeProposalError(ctx, w, err)
		return
	}
	writeJSON(ctx, w, http.StatusOK, preview)
}

// handleCreateProposal guarda una propuesta y se la manda al agent:
// POST /v1/servers/{id}/proposals. No pide código TOTP: no cambia nada en el
// servidor hasta que una persona la acepta ahí.
func (s *Server) handleCreateProposal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)
	ip := s.clientIP(r)
	session := SessionFrom(ctx)
	if !s.proposalLimiter.allow(ip) {
		writeJSON(ctx, w, http.StatusTooManyRequests, errorResponse{Error: "rate_limited", Message: "demasiadas propuestas seguidas"})
		return
	}
	server, ok := s.serverFromPath(w, r)
	if !ok {
		return
	}
	req, ok := decodeProposal(w, r)
	if !ok {
		return
	}

	userID := session.UserID
	created, err := s.proposals.Create(ctx, server.ID, req, proposals.Actor{UserID: &userID, Name: session.Username, Type: string(audit.ActorUser)})
	if err != nil {
		if errors.Is(err, proposals.ErrInvalid) || errors.Is(err, proposals.ErrDeclared) {
			s.record(ctx, audit.Entry{
				Actor: session.Username, ActorType: audit.ActorUser, Action: audit.ActionProjectPropose,
				TargetType: "server", TargetID: server.ID.String(), Result: "rejected", IP: ip,
				Detail: map[string]any{"servidor": server.Name, "proyecto": req.Name, "motivo": err.Error()},
			})
		}
		s.writeProposalError(ctx, w, err)
		return
	}
	s.record(ctx, audit.Entry{
		Actor: session.Username, ActorType: audit.ActorUser, Action: audit.ActionProjectPropose,
		TargetType: "server", TargetID: server.ID.String(), Result: audit.ResultOK, IP: ip,
		Detail: map[string]any{"servidor": server.Name, "proyecto": created.Name, "propuesta": created.ID.String(), "runtime": created.Runtime, "entorno": created.Environment},
	})
	s.log.InfoContext(ctx, "proyecto propuesto", "usuario", session.Username, "servidor", server.Name, "proyecto", created.Name, "propuesta", created.ID)

	// Si el agent está conectado, la recibe ya; si no, al reconectar.
	s.sendProposal(ctx, server.ID, proposals.Message(created))
	s.publishProposals(ctx, server.ID)

	detail, err := detailOf(created)
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo armar la vista previa", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	writeJSON(ctx, w, http.StatusCreated, detail)
}

// handleListProposals devuelve las propuestas de un servidor:
// GET /v1/servers/{id}/proposals.
func (s *Server) handleListProposals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)
	server, ok := s.serverFromPath(w, r)
	if !ok {
		return
	}
	rows, err := s.proposals.List(ctx, server.ID)
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudieron leer las propuestas", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	out := proposalListResponse{Proposals: make([]proposalSummary, 0, len(rows))}
	for _, p := range rows {
		out.Proposals = append(out.Proposals, summaryOf(p))
	}
	writeJSON(ctx, w, http.StatusOK, out)
}

// handleGetProposal devuelve una propuesta con lo que se va a escribir:
// GET /v1/proposals/{id}.
func (s *Server) handleGetProposal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)
	p, ok := s.proposalFromPath(w, r)
	if !ok {
		return
	}
	detail, err := detailOf(store.ProjectProposal{
		ID: p.ID, ServerID: p.ServerID, Name: p.Name, Runtime: p.Runtime, Environment: p.Environment,
		Spec: p.Spec, SpecHash: p.SpecHash, Status: p.Status, Message: p.Message, CreatedBy: p.CreatedBy,
		CreatedByName: p.CreatedByName, ActorType: p.ActorType, CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt,
		DeliveredAt: p.DeliveredAt, ClosedAt: p.ClosedAt,
	})
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo armar la vista previa", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	writeJSON(ctx, w, http.StatusOK, detail)
}

// handleWithdrawProposal retira una propuesta abierta:
// DELETE /v1/proposals/{id}.
func (s *Server) handleWithdrawProposal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)
	session := SessionFrom(ctx)
	p, ok := s.proposalFromPath(w, r)
	if !ok {
		return
	}
	closed, err := s.proposals.Withdraw(ctx, p.ID)
	if errors.Is(err, proposals.ErrNotOpen) {
		writeJSON(ctx, w, http.StatusConflict, errorResponse{Error: "not_open", Message: "la propuesta ya no está abierta"})
		return
	}
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo retirar la propuesta", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	s.record(ctx, audit.Entry{
		Actor: session.Username, ActorType: audit.ActorUser, Action: audit.ActionProjectWithdraw,
		TargetType: "server", TargetID: p.ServerID.String(), Result: audit.ResultOK, IP: s.clientIP(r),
		Detail: map[string]any{"servidor": p.ServerName, "proyecto": p.Name, "propuesta": p.ID.String()},
	})
	s.sendProposal(ctx, p.ServerID, proposals.WithdrawMessage(p.ID, p.Name))
	s.publishProposals(ctx, p.ServerID)
	writeJSON(ctx, w, http.StatusOK, summaryOf(closed))
}

func detailOf(p store.ProjectProposal) (proposalDetail, error) {
	preview, err := proposals.PreviewOf(p)
	if err != nil {
		return proposalDetail{}, err
	}
	return proposalDetail{proposalSummary: summaryOf(p), Preview: preview}, nil
}

// writeProposalError contesta un error del alta: una propuesta inválida dice
// qué campo y qué regla, como el parser.
func (s *Server) writeProposalError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, proposals.ErrInvalid):
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_proposal", Message: err.Error()})
	case errors.Is(err, proposals.ErrDeclared):
		writeJSON(ctx, w, http.StatusConflict, errorResponse{Error: "already_declared", Message: err.Error()})
	default:
		s.log.ErrorContext(ctx, "no se pudo guardar la propuesta", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
	}
}

// serverFromPath busca el servidor de la ruta.
func (s *Server) serverFromPath(w http.ResponseWriter, r *http.Request) (store.Server, bool) {
	ctx := r.Context()
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el ID del servidor no es un UUID"})
		return store.Server{}, false
	}
	server, err := s.queries.GetServerByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(ctx, w, http.StatusNotFound, errorResponse{Error: "not_found", Message: "no existe ese servidor"})
		return store.Server{}, false
	}
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo buscar el servidor", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return store.Server{}, false
	}
	return server, true
}

// proposalFromPath busca la propuesta de la ruta.
func (s *Server) proposalFromPath(w http.ResponseWriter, r *http.Request) (store.GetProposalRow, bool) {
	ctx := r.Context()
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el ID de la propuesta no es un UUID"})
		return store.GetProposalRow{}, false
	}
	p, err := s.proposals.Get(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(ctx, w, http.StatusNotFound, errorResponse{Error: "not_found", Message: "no existe esa propuesta"})
		return store.GetProposalRow{}, false
	}
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo buscar la propuesta", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return store.GetProposalRow{}, false
	}
	return p, true
}

// sendProposal le manda una propuesta (o su retiro) al agent, si está
// conectado. Si no lo está, o la escritura falla, la propuesta sigue abierta
// y sale con el próximo projects_state.
func (s *Server) sendProposal(ctx context.Context, serverID uuid.UUID, message proto.ProjectProposal) {
	conn := s.hub.get(serverID)
	if conn == nil {
		return
	}
	if err := s.writeMessage(context.WithoutCancel(ctx), conn, message); err != nil {
		s.log.WarnContext(ctx, "no se pudo mandar la propuesta al agent", "propuesta", message.ProposalID, "error", err)
	}
}

// publishProposals avisa a los paneles que miran el servidor.
func (s *Server) publishProposals(ctx context.Context, serverID uuid.UUID) {
	if !s.panels.hasSubscribers(serverID) {
		return
	}
	raw, err := json.Marshal(panelProposalsMessage{Type: panelProposals, ServerID: serverID})
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo armar el aviso de propuestas", "error", err)
		return
	}
	s.panels.publish(serverID, raw)
}

// handleProposalAck anota lo que hizo el agent con una propuesta.
func (s *Server) handleProposalAck(ctx context.Context, server store.Server, envelope proto.Envelope) {
	ack, err := proto.Decode[proto.ProjectProposalAck](envelope)
	if err != nil {
		s.log.WarnContext(ctx, "acuse de propuesta inválido", "servidor", server.Name, "error", err)
		return
	}
	changed := false
	switch ack.Result {
	case proto.ProposalStored:
		changed, err = s.proposals.Delivered(ctx, server.ID, ack.ProposalID)
	case proto.ProposalRejected:
		// Solo una propuesta de este servidor: el ID lo eligió el control
		// plane, pero el acuse lo manda el agent.
		p, getErr := s.proposals.Get(ctx, ack.ProposalID)
		if getErr != nil || p.ServerID != server.ID {
			return
		}
		_, err = s.proposals.Rejected(ctx, ack.ProposalID, ack.Error)
		changed = err == nil
		if errors.Is(err, proposals.ErrNotOpen) {
			err = nil
		}
	}
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo anotar el acuse de la propuesta", "servidor", server.Name, "error", err)
		return
	}
	s.log.InfoContext(ctx, "propuesta en el agent", "servidor", server.Name, "proyecto", ack.Name, "resultado", ack.Result, "error", ack.Error)
	if changed {
		s.publishProposals(ctx, server.ID)
	}
}

// reconcileProposals compara las propuestas que tiene el agent con las
// abiertas: anota las aceptadas y las entregadas, manda las que le faltan y le
// pide borrar las que ya no están abiertas.
func (s *Server) reconcileProposals(ctx context.Context, conn *websocket.Conn, server store.Server, state proto.ProjectsState) {
	r, err := s.proposals.Reconcile(ctx, server.ID, state)
	if err != nil {
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "no se pudieron comparar las propuestas", "servidor", server.Name, "error", err)
		}
		return
	}
	for _, a := range r.Accepted {
		p := a.Proposal
		s.record(ctx, audit.Entry{
			Actor: server.Name, ActorType: audit.ActorAgent, Action: audit.ActionProjectAccept,
			TargetType: "server", TargetID: server.ID.String(), Result: audit.ResultOK,
			Detail: map[string]any{"servidor": server.Name, "proyecto": p.Name, "propuesta": p.ID.String(), "igual_a_la_propuesta": a.Same},
		})
		s.log.InfoContext(ctx, "propuesta aceptada en el servidor", "servidor", server.Name, "proyecto", p.Name, "igual", a.Same)
	}
	for _, p := range r.Send {
		if err := s.writeMessage(ctx, conn, proposals.Message(p)); err != nil {
			s.log.WarnContext(ctx, "no se pudo mandar la propuesta al agent", "propuesta", p.ID, "error", err)
			return
		}
	}
	for _, p := range r.Withdraw {
		if err := s.writeMessage(ctx, conn, proposals.WithdrawMessage(p.ProposalID, p.Name)); err != nil {
			s.log.WarnContext(ctx, "no se pudo pedir que se borre una propuesta", "propuesta", p.ProposalID, "error", err)
			return
		}
	}
	if len(r.Accepted) > 0 || r.Delivered > 0 {
		s.publishProposals(ctx, server.ID)
	}
}

// RunProposalExpiry vence, cada tanto, las propuestas que nadie aceptó.
// Corre hasta que se cancela el contexto.
func (s *Server) RunProposalExpiry(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.expireProposals(ctx)
		}
	}
}

func (s *Server) expireProposals(ctx context.Context) {
	expired, err := s.proposals.Expire(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "no se pudieron vencer las propuestas", "error", err)
		}
		return
	}
	for _, p := range expired {
		s.sendProposal(ctx, p.ServerID, proposals.WithdrawMessage(p.ID, p.Name))
		s.publishProposals(ctx, p.ServerID)
		s.log.InfoContext(ctx, "propuesta vencida", "proyecto", p.Name, "propuesta", p.ID)
	}
}
