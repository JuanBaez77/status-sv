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
	"golang.org/x/time/rate"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/audit"
	"github.com/JuanBaez77/status-sv/server/internal/deploys"
	"github.com/JuanBaez77/status-sv/server/internal/store"
	"github.com/JuanBaez77/status-sv/server/internal/users"
)

// Deploys desde el panel (ADR-015, ADR-017).
//
// Un deploy se pide con POST /v1/projects/{id}/deploys (o /rollback). El
// control plane lo anota como pendiente, se lo manda al agent con el mismo
// ID y contesta con lo que dijo el agent: aceptado o rechazado. El avance
// llega después por deploy_event y se empuja a los paneles.

// Límites de los pedidos y de las fotos.
const (
	// deployEventsPerSecond: el agent manda una foto por cambio de paso y
	// cada 5 s durante un paso largo. Más que esto es un error.
	deployEventsPerSecond = rate.Limit(2)
	deployEventsBurst     = 30
	// deployRequestsPerMinute acota los pedidos de deploy por IP.
	deployRequestsPerMinute = 10
	deployRequestsBurst     = 5
	// maxDeployBody acota el pedido: son tres campos.
	maxDeployBody = 4 << 10
	// historyLimit es cuántos deploys devuelve la historia de un proyecto.
	historyLimit = 50
	// expireEvery es cada cuánto se cierran los pendientes sin respuesta.
	expireEvery = 30 * time.Second
)

// panelDeploy es el aviso del WebSocket del panel de que cambió un deploy.
const panelDeploy = "deploy"

type panelDeployMessage struct {
	Type      string     `json:"type"`
	ServerID  uuid.UUID  `json:"server_id"`
	ProjectID *uuid.UUID `json:"project_id"`
	DeployID  uuid.UUID  `json:"deploy_id"`
	Status    string     `json:"status"`
}

// deployRequest es el pedido del panel. Confirm es el nombre del proyecto,
// escrito a mano; TOTPCode, un código nuevo de la app (solo producción).
type deployRequest struct {
	Commit   string `json:"commit"`
	Confirm  string `json:"confirm"`
	TOTPCode string `json:"totp_code"`
}

// deploySummary es un deploy como lo muestra la historia.
type deploySummary struct {
	ID              uuid.UUID  `json:"id"`
	Kind            string     `json:"kind"`
	RequestedCommit string     `json:"requested_commit,omitempty"`
	Commit          string     `json:"commit,omitempty"`
	PreviousCommit  string     `json:"previous_commit,omitempty"`
	Status          string     `json:"status"`
	Code            string     `json:"code,omitempty"`
	Message         string     `json:"message,omitempty"`
	RequestedBy     string     `json:"requested_by"`
	RequestedAt     time.Time  `json:"requested_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

// deployDetail es un deploy con sus pasos.
type deployDetail struct {
	deploySummary
	ProjectID   *uuid.UUID         `json:"project_id"`
	ProjectName string             `json:"project_name"`
	ServerID    uuid.UUID          `json:"server_id"`
	ServerName  string             `json:"server_name"`
	Environment string             `json:"environment"`
	Steps       []proto.DeployStep `json:"steps"`
}

type deployHistoryResponse struct {
	Deploys []deploySummary `json:"deploys"`
}

func detailFrom(d store.GetDeployRow) deployDetail {
	return deployDetail{
		deploySummary: deploySummary{
			ID: d.ID, Kind: d.Kind, RequestedCommit: d.RequestedCommit, Commit: d.DeployedCommit,
			PreviousCommit: d.PreviousCommit, Status: d.Status, Code: d.Code, Message: d.Message,
			RequestedBy: d.RequestedByName, RequestedAt: d.RequestedAt, StartedAt: d.StartedAt, FinishedAt: d.FinishedAt,
		},
		ProjectID: d.ProjectID, ProjectName: d.ProjectName, ServerID: d.ServerID, ServerName: d.ServerName,
		Environment: d.Environment, Steps: deploys.Steps(d.Steps),
	}
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	s.startDeploy(w, r, proto.DeployKindDeploy)
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	s.startDeploy(w, r, proto.DeployKindRollback)
}

// startDeploy atiende POST /v1/projects/{id}/deploys y /rollback.
func (s *Server) startDeploy(w http.ResponseWriter, r *http.Request, kind proto.DeployKind) {
	ctx := r.Context()
	noStore(w)
	ip := s.clientIP(r)
	session := SessionFrom(ctx)

	if !s.deployLimiter.allow(ip) {
		writeJSON(ctx, w, http.StatusTooManyRequests, errorResponse{Error: "rate_limited", Message: "demasiados pedidos de deploy seguidos"})
		return
	}
	projectID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el ID del proyecto no es un UUID"})
		return
	}
	var req deployRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDeployBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el pedido no es un JSON válido"})
		return
	}
	if kind == proto.DeployKindRollback && req.Commit != "" {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "un rollback vuelve al estado bueno anterior; no lleva commit"})
		return
	}
	if req.Commit != "" && !proto.ValidCommit(req.Commit) {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el commit tiene que ser un SHA completo de 40 caracteres en minúscula"})
		return
	}

	project, err := s.queries.GetProject(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(ctx, w, http.StatusNotFound, errorResponse{Error: "not_found", Message: "no existe ese proyecto"})
		return
	}
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo buscar el proyecto", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}

	action := audit.ActionDeployRequest
	if kind == proto.DeployKindRollback {
		action = audit.ActionRollbackRequest
	}
	record := func(result string, detail map[string]any) {
		detail["servidor"], detail["proyecto"] = project.ServerName, project.Name
		if req.Commit != "" {
			detail["commit"] = req.Commit
		}
		s.record(ctx, audit.Entry{
			Actor: session.Username, ActorType: audit.ActorUser, Action: action,
			TargetType: "project", TargetID: project.ID.String(), Result: result, IP: ip, Detail: detail,
		})
	}

	// La confirmación: el nombre del proyecto, escrito a mano (ADR-017).
	if req.Confirm != project.Name {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "confirmation_required", Message: "para confirmar hay que escribir el nombre del proyecto"})
		return
	}
	if project.Runtime != string(proto.RuntimeDocker) {
		writeJSON(ctx, w, http.StatusConflict, errorResponse{Error: "unsupported", Message: "el deploy de proyectos de systemd llega en el Sprint 5"})
		return
	}
	// En producción, además, un código TOTP nuevo.
	if project.Environment == string(proto.EnvProduction) {
		if !s.totpLimiter.allow(ip) {
			tooManyTOTP(w, r)
			return
		}
		if err := s.users.ConfirmAction(ctx, session, req.TOTPCode); err != nil {
			if errors.Is(err, users.ErrInvalidCode) || errors.Is(err, users.ErrWrongStage) {
				record("rejected", map[string]any{"motivo": "código TOTP inválido"})
				writeJSON(ctx, w, http.StatusForbidden, errorResponse{Error: "invalid_code", Message: "en producción hace falta un código nuevo de la app de autenticación"})
				return
			}
			s.log.ErrorContext(ctx, "no se pudo verificar el código", "error", err)
			writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
			return
		}
	}

	if s.hub.get(project.ServerID) == nil {
		record("rejected", map[string]any{"motivo": "agent desconectado"})
		writeJSON(ctx, w, http.StatusConflict, errorResponse{Error: "agent_offline", Message: "el agent de ese servidor no está conectado"})
		return
	}

	id, err := uuid.NewV7()
	if err != nil {
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	created, err := s.deploys.Create(ctx, deploys.Request{
		ID: id, ServerID: project.ServerID, ProjectID: project.ID, ProjectName: project.Name,
		Environment: project.Environment, Kind: kind, Commit: req.Commit, UserID: session.UserID, Username: session.Username,
	})
	if errors.Is(err, deploys.ErrActive) {
		record("rejected", map[string]any{"motivo": "ya hay un deploy en curso"})
		writeJSON(ctx, w, http.StatusConflict, errorResponse{Error: "deploy_in_progress", Message: "ese proyecto ya tiene un deploy en curso"})
		return
	}
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo anotar el deploy", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	s.publishDeploy(ctx, created.ServerID, created.ProjectID, created.ID, created.Status)

	cmd := proto.Command{Operation: proto.OpDeploy, Deploy: &proto.DeployParams{Project: project.Name, Commit: req.Commit}}
	if kind == proto.DeployKindRollback {
		cmd = proto.Command{Operation: proto.OpRollback, Rollback: &proto.RollbackParams{Project: project.Name}}
	}
	ack, err := s.sendCommandWithID(ctx, project.ServerID, id, cmd)
	detail := map[string]any{"deploy": id.String()}
	switch {
	case errors.Is(err, errNoAnswer):
		// Puede que el agent lo haya recibido: queda pendiente, y si no manda
		// ninguna foto se cierra solo (deploys.PendingTimeout).
		record("no_answer", detail)
	case err != nil:
		s.log.WarnContext(ctx, "no se pudo mandar el deploy al agent", "deploy", id, "error", err)
		s.rejectDeploy(ctx, id, "agent_offline", "no se pudo mandar el pedido al agent")
		record("rejected", map[string]any{"deploy": id.String(), "motivo": "agent desconectado"})
	case ack.Status != proto.CommandOK:
		s.rejectDeploy(ctx, id, string(ack.Code), ack.Message)
		detail["codigo"] = string(ack.Code)
		record("rejected", detail)
	default:
		record("accepted", detail)
	}

	s.log.InfoContext(ctx, "deploy pedido",
		"usuario", session.Username, "servidor", project.ServerName, "proyecto", project.Name,
		"tipo", kind, "deploy", id, "estado", ack.Status, "codigo", ack.Code, "error", err)

	current, err := s.deploys.Get(ctx, id)
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo leer el deploy", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	writeJSON(ctx, w, http.StatusCreated, detailFrom(current))
}

// rejectDeploy cierra un deploy que el agent no aceptó y avisa a los paneles.
func (s *Server) rejectDeploy(ctx context.Context, id uuid.UUID, code, message string) {
	d, changed, err := s.deploys.Reject(ctx, id, code, message)
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo cerrar el deploy rechazado", "deploy", id, "error", err)
		return
	}
	if changed {
		s.publishDeploy(ctx, d.ServerID, d.ProjectID, d.ID, d.Status)
	}
}

// handleProjectDeploys devuelve la historia de un proyecto:
// GET /v1/projects/{id}/deploys.
func (s *Server) handleProjectDeploys(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)
	projectID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el ID del proyecto no es un UUID"})
		return
	}
	project, err := s.queries.GetProject(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(ctx, w, http.StatusNotFound, errorResponse{Error: "not_found", Message: "no existe ese proyecto"})
		return
	}
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo buscar el proyecto", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	rows, err := s.deploys.History(ctx, project.ServerID, project.Name, historyLimit)
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo leer la historia de deploys", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	out := deployHistoryResponse{Deploys: make([]deploySummary, 0, len(rows))}
	for _, d := range rows {
		out.Deploys = append(out.Deploys, deploySummary{
			ID: d.ID, Kind: d.Kind, RequestedCommit: d.RequestedCommit, Commit: d.DeployedCommit,
			PreviousCommit: d.PreviousCommit, Status: d.Status, Code: d.Code, Message: d.Message,
			RequestedBy: d.RequestedByName, RequestedAt: d.RequestedAt, StartedAt: d.StartedAt, FinishedAt: d.FinishedAt,
		})
	}
	writeJSON(ctx, w, http.StatusOK, out)
}

// handleGetDeploy devuelve un deploy con sus pasos: GET /v1/deploys/{id}.
func (s *Server) handleGetDeploy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el ID del deploy no es un UUID"})
		return
	}
	d, err := s.deploys.Get(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(ctx, w, http.StatusNotFound, errorResponse{Error: "not_found", Message: "no existe ese deploy"})
		return
	}
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo leer el deploy", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	writeJSON(ctx, w, http.StatusOK, detailFrom(d))
}

// handleDeployEvent guarda la foto de un deploy que manda el agent y le
// contesta, como con las otras fotos:
//
//   - deploy_event_ack: guardada, o más vieja que la guardada.
//   - invalid_message: no se va a aceptar nunca (un deploy que no existe o
//     que no es de este servidor); la descarta.
//   - rate_limited o internal: la vuelve a mandar más tarde.
func (s *Server) handleDeployEvent(ctx context.Context, conn *websocket.Conn, server store.Server, limiter *rate.Limiter, envelope proto.Envelope) error {
	if !limiter.Allow() {
		s.log.WarnContext(ctx, "foto de deploy rechazada por frecuencia", "servidor", server.Name)
		s.replyError(ctx, conn, envelope.ID, proto.CodeRateLimited, "demasiadas fotos de deploy seguidas")
		return nil
	}
	ev, err := proto.Decode[proto.DeployEvent](envelope)
	if err != nil {
		s.log.WarnContext(ctx, "foto de deploy inválida", "servidor", server.Name, "error", err)
		s.replyError(ctx, conn, envelope.ID, proto.CodeInvalidMessage, err.Error())
		return nil
	}

	ingestCtx, cancel := context.WithTimeout(ctx, ingestTimeout)
	defer cancel()
	d, applied, err := s.deploys.Apply(ingestCtx, server.ID, ev)
	switch {
	case errors.Is(err, deploys.ErrUnknownDeploy), errors.Is(err, deploys.ErrActive):
		s.log.WarnContext(ctx, "foto de deploy descartada", "servidor", server.Name, "deploy", ev.DeployID, "error", err)
		s.replyError(ctx, conn, envelope.ID, proto.CodeInvalidMessage, err.Error())
		return nil
	case err != nil:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.log.ErrorContext(ctx, "no se pudo guardar la foto del deploy", "servidor", server.Name, "error", err)
		s.replyError(ctx, conn, envelope.ID, proto.CodeInternal, "no se pudo guardar la foto; reintentar más tarde")
		return nil
	}

	if applied {
		s.publishDeploy(ctx, server.ID, d.ProjectID, d.ID, d.Status)
		if deploys.Final(d.Status) {
			target := d.ProjectName
			if d.ProjectID != nil {
				target = d.ProjectID.String()
			}
			detail := map[string]any{"deploy": d.ID.String(), "proyecto": d.ProjectName, "tipo": d.Kind}
			if d.DeployedCommit != "" {
				detail["commit"] = d.DeployedCommit
			}
			if d.Code != "" {
				detail["codigo"] = d.Code
			}
			s.record(ctx, audit.Entry{
				Actor: server.Name, ActorType: audit.ActorAgent, Action: audit.ActionDeployFinish,
				TargetType: "project", TargetID: target, Result: d.Status, Detail: detail,
			})
			s.log.InfoContext(ctx, "deploy terminado", "servidor", server.Name, "proyecto", d.ProjectName,
				"deploy", d.ID, "estado", d.Status, "codigo", d.Code)
		}
	}
	return s.writeMessage(ctx, conn, proto.DeployEventAck{DeployID: ev.DeployID, Seq: ev.Seq})
}

// publishDeploy avisa a los paneles que miran el servidor que cambió un
// deploy. El panel vuelve a pedir lo que está mirando.
func (s *Server) publishDeploy(ctx context.Context, serverID uuid.UUID, projectID *uuid.UUID, deployID uuid.UUID, status string) {
	if !s.panels.hasSubscribers(serverID) {
		return
	}
	raw, err := json.Marshal(panelDeployMessage{Type: panelDeploy, ServerID: serverID, ProjectID: projectID, DeployID: deployID, Status: status})
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo armar el aviso del deploy", "error", err)
		return
	}
	s.panels.publish(serverID, raw)
}

// record escribe un renglón de auditoría. Si no se puede, se registra en el
// log con todo el renglón: la acción ya pasó y no se deshace.
func (s *Server) record(ctx context.Context, e audit.Entry) {
	if err := s.audit.Record(context.WithoutCancel(ctx), e); err != nil {
		s.log.ErrorContext(ctx, "no se pudo escribir la auditoría", "error", err,
			"actor", e.Actor, "accion", e.Action, "objetivo", e.TargetID, "resultado", e.Result)
	}
}

// RunDeployExpiry cierra, cada tanto, los deploys pendientes a los que el
// agent nunca contestó. Corre hasta que se cancela el contexto.
func (s *Server) RunDeployExpiry(ctx context.Context) {
	ticker := time.NewTicker(expireEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.expireDeploys(ctx)
		}
	}
}

func (s *Server) expireDeploys(ctx context.Context) {
	expired, err := s.deploys.Expire(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "no se pudieron cerrar los deploys pendientes", "error", err)
		}
		return
	}
	for _, d := range expired {
		s.publishDeploy(ctx, d.ServerID, d.ProjectID, d.ID, d.Status)
		s.record(ctx, audit.Entry{
			Actor: "control-plane", ActorType: audit.ActorSystem, Action: audit.ActionDeployFinish,
			TargetType: "project", TargetID: d.ProjectName, Result: d.Status,
			Detail: map[string]any{"deploy": d.ID.String(), "codigo": d.Code},
		})
		s.log.WarnContext(ctx, "deploy cerrado sin respuesta del agent", "deploy", d.ID, "proyecto", d.ProjectName)
	}
}
