package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/approvals"
	"github.com/JuanBaez77/status-sv/server/internal/audit"
	"github.com/JuanBaez77/status-sv/server/internal/store"
	"github.com/JuanBaez77/status-sv/server/internal/tokens"
	"github.com/JuanBaez77/status-sv/server/internal/users"
)

// Las aprobaciones de los pedidos de escritura del MCP (ADR-026).
const (
	// approvalSweepEvery es cada cuánto se vencen y se cancelan los pedidos.
	approvalSweepEvery = 30 * time.Second
	// maxApprovalBody acota el pedido de aprobar o rechazar.
	maxApprovalBody = 2 << 10
	// systemActor es quien vence o cancela un pedido.
	systemActor = "control-plane"
	// runningHistory es cuánta historia se mira para saber qué commit corre.
	runningHistory = 20
)

// approvalBody es un pedido de escritura como lo ve el panel.
type approvalBody struct {
	ID          uuid.UUID  `json:"id"`
	Operation   string     `json:"operation"`
	ServerID    uuid.UUID  `json:"server_id"`
	Server      string     `json:"server"`
	ProjectID   *uuid.UUID `json:"project_id"`
	Project     string     `json:"project"`
	Environment string     `json:"environment"`
	Commit      string     `json:"commit,omitempty"`
	Unit        string     `json:"unit,omitempty"`
	// Reason es texto del LLM: el panel lo muestra aparte, como no confiable.
	Reason       string     `json:"reason,omitempty"`
	RequestedBy  string     `json:"requested_by"`
	TokenPrefix  string     `json:"token_prefix"`
	RequestedIP  string     `json:"requested_ip"`
	RequestedAt  time.Time  `json:"requested_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	Status       string     `json:"status"`
	DecidedAt    *time.Time `json:"decided_at"`
	DecidedBy    string     `json:"decided_by,omitempty"`
	DecisionNote string     `json:"decision_note,omitempty"`
	DeployID     *uuid.UUID `json:"deploy_id"`
	// Repo y RunningCommit ayudan a decidir un pendiente: a qué repo apunta el
	// commit y qué commit corre ahora.
	Repo          string `json:"repo,omitempty"`
	RunningCommit string `json:"running_commit,omitempty"`
}

type approvalListResponse struct {
	Approvals []approvalBody `json:"approvals"`
}

// approveRequest es aprobar: el nombre del proyecto escrito y un código TOTP
// nuevo, como un deploy de producción desde el panel (ADR-017).
type approveRequest struct {
	Confirm  string `json:"confirm"`
	TOTPCode string `json:"totp_code"`
}

type rejectRequest struct {
	Note string `json:"note"`
}

// approvalFrom arma la respuesta. Un pendiente cuyo plazo pasó se muestra
// vencido aunque el barrido todavía no lo haya marcado.
func approvalFrom(a approvals.Approval, now time.Time) approvalBody {
	status := string(a.Status)
	if a.Status == approvals.StatusPending && !now.Before(a.ExpiresAt) {
		status = string(approvals.StatusExpired)
	}
	return approvalBody{
		ID: a.ID, Operation: string(a.Operation), ServerID: a.ServerID, Server: a.ServerName,
		ProjectID: a.ProjectID, Project: a.ProjectName, Environment: a.Environment,
		Commit: a.Commit, Unit: a.Unit, Reason: a.Reason,
		RequestedBy: a.RequestedByName, TokenPrefix: a.RequestedByPrefix, RequestedIP: a.RequestedIP,
		RequestedAt: a.RequestedAt, ExpiresAt: a.ExpiresAt, Status: status,
		DecidedAt: a.DecidedAt, DecidedBy: a.DecidedByName, DecisionNote: a.DecisionNote, DeployID: a.DeployID,
	}
}

// withContext suma lo que ayuda a decidir un pendiente: el repo del proyecto
// y el commit que corre.
func (s *Server) withContext(ctx context.Context, body approvalBody) approvalBody {
	if body.Status != string(approvals.StatusPending) || body.ProjectID == nil {
		return body
	}
	project, err := s.queries.GetProject(ctx, *body.ProjectID)
	if err != nil {
		return body
	}
	body.Repo = project.Repo
	body.RunningCommit = s.runningCommit(ctx, project.ServerID, project.Name)
	return body
}

// runningCommit es el commit que corre según la historia: el del último
// deploy que quedó bien, o el anterior de uno que volvió atrás. Vacío si no
// se sabe.
func (s *Server) runningCommit(ctx context.Context, serverID uuid.UUID, project string) string {
	rows, err := s.deploys.History(ctx, serverID, project, runningHistory)
	if err != nil {
		return ""
	}
	for _, d := range rows {
		switch {
		case d.Status == string(proto.DeploySucceeded) && d.DeployedCommit != "":
			return d.DeployedCommit
		case d.Status == string(proto.DeployRolledBack) && d.PreviousCommit != "":
			return d.PreviousCommit
		}
	}
	return ""
}

// handleListApprovals devuelve los pedidos: GET /v1/approvals?status=&limit=.
func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)
	status := approvals.Status(r.URL.Query().Get("status"))
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > approvals.MaxListLimit {
			writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "limit va de 1 a 200"})
			return
		}
		limit = n
	}
	list, err := s.approvals.List(ctx, status, limit)
	if errors.Is(err, approvals.ErrInvalid) {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "estado desconocido"})
		return
	}
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudieron listar los pedidos", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	now := time.Now()
	out := approvalListResponse{Approvals: make([]approvalBody, 0, len(list))}
	for _, a := range list {
		out.Approvals = append(out.Approvals, s.withContext(ctx, approvalFrom(a, now)))
	}
	writeJSON(ctx, w, http.StatusOK, out)
}

// handleGetApproval devuelve un pedido: GET /v1/approvals/{id}.
func (s *Server) handleGetApproval(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)
	a, ok := s.loadApproval(w, r)
	if !ok {
		return
	}
	writeJSON(ctx, w, http.StatusOK, s.withContext(ctx, approvalFrom(a, time.Now())))
}

func (s *Server) loadApproval(w http.ResponseWriter, r *http.Request) (approvals.Approval, bool) {
	ctx := r.Context()
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el ID del pedido no es un UUID"})
		return approvals.Approval{}, false
	}
	a, err := s.approvals.Get(ctx, id)
	if errors.Is(err, approvals.ErrNotFound) {
		writeJSON(ctx, w, http.StatusNotFound, errorResponse{Error: "not_found", Message: "no existe ese pedido"})
		return approvals.Approval{}, false
	}
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo buscar el pedido", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return approvals.Approval{}, false
	}
	return a, true
}

// capabilityFor es el permiso que tiene que seguir teniendo el token para
// que su pedido se pueda aprobar.
func capabilityFor(op approvals.Operation) tokens.Capability {
	switch op {
	case approvals.OpRollback:
		return tokens.CapRollback
	case approvals.OpRestart:
		return tokens.CapRestart
	default:
		return tokens.CapDeploy
	}
}

// kindFor es el tipo de deploy que sale de un pedido.
func kindFor(op approvals.Operation) proto.DeployKind {
	switch op {
	case approvals.OpRollback:
		return proto.DeployKindRollback
	case approvals.OpRestart:
		return proto.DeployKindRestart
	case approvals.OpBootstrap:
		return proto.DeployKindBootstrap
	default:
		return proto.DeployKindDeploy
	}
}

// requestActionFor es la acción de auditoría del deploy que sale.
func requestActionFor(kind proto.DeployKind) string {
	switch kind {
	case proto.DeployKindRollback:
		return audit.ActionRollbackRequest
	case proto.DeployKindBootstrap:
		return audit.ActionBootstrapRequest
	case proto.DeployKindRestart:
		return audit.ActionRestartRequest
	default:
		return audit.ActionDeployRequest
	}
}

// handleApproveApproval aprueba un pedido y manda lo que pide:
// POST /v1/approvals/{id}/approve. Antes vuelve a verificar todo: el token
// que lo pidió, el proyecto, el agent y que no haya otro deploy en curso.
func (s *Server) handleApproveApproval(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)
	ip := s.clientIP(r)
	session := SessionFrom(ctx)

	if !s.deployLimiter.allow(ip) {
		writeJSON(ctx, w, http.StatusTooManyRequests, errorResponse{Error: "rate_limited", Message: "demasiados pedidos seguidos"})
		return
	}
	var req approveRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxApprovalBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el pedido no es un JSON válido"})
		return
	}
	a, ok := s.loadApproval(w, r)
	if !ok {
		return
	}
	now := time.Now()
	if a.Status != approvals.StatusPending || !now.Before(a.ExpiresAt) {
		writeJSON(ctx, w, http.StatusConflict, errorResponse{Error: "not_pending", Message: "el pedido ya no está pendiente"})
		return
	}

	record := func(result string, detail map[string]any) {
		detail["operacion"], detail["servidor"], detail["proyecto"] = string(a.Operation), a.ServerName, a.ProjectName
		detail["pedido_por"] = a.RequestedByName
		s.record(ctx, audit.Entry{
			Actor: session.Username, ActorType: audit.ActorUser, Action: audit.ActionApprovalApprove,
			TargetType: "approval", TargetID: a.ID.String(), Result: result, IP: ip, Detail: detail,
		})
	}

	// El proyecto y el token: si alguno ya no sirve, el pedido se cancela.
	project, reason := s.approvalProject(ctx, a)
	if reason == "" {
		reason = s.approvalToken(ctx, a, now)
	}
	if reason != "" {
		s.cancelApproval(ctx, a, reason)
		writeJSON(ctx, w, http.StatusConflict, errorResponse{Error: "cancelled", Message: "el pedido se canceló: " + reason})
		return
	}

	if req.Confirm != a.ProjectName {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "confirmation_required", Message: "para aprobar hay que escribir el nombre del proyecto"})
		return
	}
	if !s.totpLimiter.allow(ip) {
		tooManyTOTP(w, r)
		return
	}
	if err := s.users.ConfirmAction(ctx, session, req.TOTPCode); err != nil {
		if errors.Is(err, users.ErrInvalidCode) || errors.Is(err, users.ErrWrongStage) {
			record("rejected", map[string]any{"motivo": "código TOTP inválido"})
			writeJSON(ctx, w, http.StatusForbidden, errorResponse{Error: "invalid_code", Message: "hace falta un código nuevo de la app de autenticación"})
			return
		}
		s.log.ErrorContext(ctx, "no se pudo verificar el código", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}

	// Lo que no cancela el pedido: sigue pendiente y se puede aprobar más
	// tarde, mientras no venza.
	if s.hub.get(project.ServerID) == nil {
		writeJSON(ctx, w, http.StatusConflict, errorResponse{Error: "agent_offline", Message: "el agent de ese servidor no está conectado; el pedido sigue pendiente"})
		return
	}
	if s.deployActive(ctx, project) {
		writeJSON(ctx, w, http.StatusConflict, errorResponse{Error: "deploy_in_progress", Message: "ese proyecto tiene un deploy en curso; el pedido sigue pendiente"})
		return
	}

	approved, err := s.approvals.Approve(ctx, a.ID, approvals.Decider{UserID: session.UserID, Name: session.Username})
	if errors.Is(err, approvals.ErrNotPending) {
		writeJSON(ctx, w, http.StatusConflict, errorResponse{Error: "not_pending", Message: "el pedido ya no está pendiente"})
		return
	}
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo aprobar el pedido", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}

	kind := kindFor(a.Operation)
	deployID, failure := s.dispatch(ctx, dispatchRequest{
		project: project, kind: kind, commit: a.Commit, unit: a.Unit,
		// El deploy lo pidió el token; lo aprobó la persona, que queda en el
		// pedido y en la auditoría.
		requestedBy: a.RequestedByName,
		record: func(result string, detail map[string]any) {
			detail["servidor"], detail["proyecto"], detail["aprobacion"] = project.ServerName, project.Name, a.ID.String()
			detail["pedido_por"] = a.RequestedByName
			if a.Commit != "" {
				detail["commit"] = a.Commit
			}
			if a.Unit != "" {
				detail["unidad"] = a.Unit
			}
			s.record(ctx, audit.Entry{
				Actor: session.Username, ActorType: audit.ActorUser, Action: requestActionFor(kind),
				TargetType: "project", TargetID: project.ID.String(), Result: result, IP: ip, Detail: detail,
			})
		},
	})
	detail := map[string]any{}
	if failure != nil {
		// Aprobado, pero no salió: se dice por qué. Para intentarlo de nuevo
		// hace falta otro pedido.
		detail["motivo"] = failure.message
		record("failed", detail)
		writeJSON(ctx, w, failure.status, errorResponse{Error: failure.code, Message: "se aprobó, pero no salió: " + failure.message})
		return
	}
	if err := s.approvals.SetDeploy(ctx, a.ID, deployID); err != nil {
		s.log.ErrorContext(ctx, "no se pudo anotar el deploy del pedido", "pedido", a.ID, "error", err)
	}
	detail["deploy"] = deployID.String()
	record("ok", detail)

	approved.DeployID = &deployID
	writeJSON(ctx, w, http.StatusOK, approvalFrom(approved, time.Now()))
}

// approvalProject devuelve el proyecto del pedido, o por qué ya no sirve.
func (s *Server) approvalProject(ctx context.Context, a approvals.Approval) (store.GetProjectRow, string) {
	if a.ProjectID == nil {
		return store.GetProjectRow{}, "el proyecto dejó de estar declarado"
	}
	project, err := s.queries.GetProject(ctx, *a.ProjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.GetProjectRow{}, "el proyecto dejó de estar declarado"
	}
	if err != nil {
		return store.GetProjectRow{}, "no se pudo leer el proyecto"
	}
	if project.Environment != a.Environment {
		return store.GetProjectRow{}, "el proyecto cambió de entorno"
	}
	return project, ""
}

// approvalToken dice por qué el token que pidió ya no alcanza, o nada.
func (s *Server) approvalToken(ctx context.Context, a approvals.Approval, now time.Time) string {
	if a.RequestedByToken == nil {
		return "el token que lo pidió ya no existe"
	}
	tok, err := s.tokens.Get(ctx, *a.RequestedByToken)
	if err != nil {
		return "el token que lo pidió ya no existe"
	}
	if tok.Status(now) != tokens.StatusActive {
		return "el token que lo pidió fue revocado o venció"
	}
	if !tok.AllowsProject(capabilityFor(a.Operation), a.Environment, *a.ProjectID) {
		return "el token que lo pidió ya no tiene el permiso"
	}
	return ""
}

// deployActive dice si el proyecto tiene un deploy pendiente o en curso.
func (s *Server) deployActive(ctx context.Context, project store.GetProjectRow) bool {
	rows, err := s.deploys.History(ctx, project.ServerID, project.Name, 1)
	if err != nil || len(rows) == 0 {
		return false
	}
	return rows[0].Status == "pending" || rows[0].Status == string(proto.DeployRunning)
}

// cancelApproval cancela un pedido que ya no se puede aprobar y lo anota.
func (s *Server) cancelApproval(ctx context.Context, a approvals.Approval, reason string) {
	cancelled, err := s.approvals.Cancel(ctx, a.ID, reason)
	if err != nil {
		if !errors.Is(err, approvals.ErrNotPending) {
			s.log.ErrorContext(ctx, "no se pudo cancelar el pedido", "pedido", a.ID, "error", err)
		}
		return
	}
	s.recordSystemDecision(ctx, audit.ActionApprovalCancel, cancelled)
}

func (s *Server) recordSystemDecision(ctx context.Context, action string, a approvals.Approval) {
	s.record(ctx, audit.Entry{
		Actor: systemActor, ActorType: audit.ActorSystem, Action: action,
		TargetType: "approval", TargetID: a.ID.String(), Result: audit.ResultOK,
		Detail: map[string]any{
			"operacion": string(a.Operation), "servidor": a.ServerName, "proyecto": a.ProjectName,
			"pedido_por": a.RequestedByName, "motivo": a.DecisionNote,
		},
	})
}

// handleRejectApproval rechaza un pedido: POST /v1/approvals/{id}/reject. No
// pide código: frenar tiene que ser fácil.
func (s *Server) handleRejectApproval(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)
	ip := s.clientIP(r)
	session := SessionFrom(ctx)

	var req rejectRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxApprovalBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el pedido no es un JSON válido"})
		return
	}
	a, ok := s.loadApproval(w, r)
	if !ok {
		return
	}
	rejected, err := s.approvals.Reject(ctx, a.ID, approvals.Decider{UserID: session.UserID, Name: session.Username}, req.Note)
	switch {
	case errors.Is(err, approvals.ErrInvalid):
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: err.Error()})
		return
	case errors.Is(err, approvals.ErrNotPending):
		writeJSON(ctx, w, http.StatusConflict, errorResponse{Error: "not_pending", Message: "el pedido ya no está pendiente"})
		return
	case err != nil:
		s.log.ErrorContext(ctx, "no se pudo rechazar el pedido", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	s.record(ctx, audit.Entry{
		Actor: session.Username, ActorType: audit.ActorUser, Action: audit.ActionApprovalReject,
		TargetType: "approval", TargetID: a.ID.String(), Result: audit.ResultOK, IP: ip,
		Detail: map[string]any{
			"operacion": string(a.Operation), "servidor": a.ServerName, "proyecto": a.ProjectName,
			"pedido_por": a.RequestedByName, "motivo": rejected.DecisionNote,
		},
	})
	writeJSON(ctx, w, http.StatusOK, approvalFrom(rejected, time.Now()))
}

// RunApprovalSweep vence y cancela, cada tanto, los pedidos que ya no se
// pueden aprobar. Corre hasta que se cancela el contexto.
func (s *Server) RunApprovalSweep(ctx context.Context) {
	ticker := time.NewTicker(approvalSweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepApprovals(ctx)
		}
	}
}

func (s *Server) sweepApprovals(ctx context.Context) {
	swept, err := s.approvals.Sweep(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "no se pudieron barrer los pedidos", "error", err)
		}
		return
	}
	for _, a := range swept.Expired {
		s.recordSystemDecision(ctx, audit.ActionApprovalExpire, a)
		s.log.InfoContext(ctx, "pedido vencido", "pedido", a.ID, "proyecto", a.ProjectName)
	}
	for _, a := range swept.Cancelled {
		s.recordSystemDecision(ctx, audit.ActionApprovalCancel, a)
		s.log.InfoContext(ctx, "pedido cancelado", "pedido", a.ID, "proyecto", a.ProjectName, "motivo", a.DecisionNote)
	}
}
