package mcp

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/approvals"
	"github.com/JuanBaez77/status-sv/server/internal/tokens"
)

// Las escrituras (ADR-026): deploy, rollback y restart guardan un pedido
// pendiente y no le mandan nada al agent. Una persona lo aprueba en el panel,
// con su segundo factor; get_approval dice en qué quedó.

// statusPendingApproval es lo que devuelve una escritura: nada se ejecutó.
const statusPendingApproval = "pending_approval"

// pendingMessage le explica al LLM qué pasa con su pedido.
const pendingMessage = "Nada se ejecutó todavía. Una persona tiene que aprobarlo en el panel de status-sv " +
	"(Aprobaciones), con su segundo factor; vence a la hora. Seguí el pedido con get_approval y, cuando " +
	"tenga deploy_id, el resultado con get_deploy."

// writes son las anotaciones de una escritura: cambia lo que corre, y pedir
// dos veces no es lo mismo que una.
func writes(title string) *sdk.ToolAnnotations {
	destructive, closed := true, false
	return &sdk.ToolAnnotations{Title: title, DestructiveHint: &destructive, OpenWorldHint: &closed}
}

func (s *Server) addWriteTools() {
	addTool(s, &sdk.Tool{
		Name: "deploy",
		Description: "Pide desplegar un commit de la rama declarada de un proyecto. No se ejecuta enseguida: " +
			"devuelve pending_approval con un ID, y una persona lo aprueba en el panel. El commit va completo " +
			"(40 caracteres): se aprueba y se despliega exactamente ese, y el agent rechaza uno que no esté en la " +
			"rama. Al aprobarse, el agent trae, compila, levanta, verifica y vuelve atrás solo si no queda bien. " +
			"Pide el permiso deploy del entorno del proyecto.",
		Annotations: writes("Pedir un deploy"),
	}, classWrite, s.requestDeploy)

	addTool(s, &sdk.Tool{
		Name: "rollback",
		Description: "Pide volver un proyecto a su estado bueno anterior. Devuelve pending_approval: una persona " +
			"lo aprueba en el panel. Un rollback no deshace cambios en la base (por ejemplo, migraciones). " +
			"Pide el permiso rollback del entorno del proyecto.",
		Annotations: writes("Pedir un rollback"),
	}, classWrite, s.requestRollback)

	addTool(s, &sdk.Tool{
		Name: "restart",
		Description: "Pide reiniciar un proyecto, o una unidad suya (un servicio de Compose, su contenedor o una " +
			"unidad de systemd del proyecto). No cambia el código ni las imágenes, y si no queda bien no vuelve " +
			"atrás. Devuelve pending_approval: una persona lo aprueba en el panel. Pide el permiso restart del " +
			"entorno del proyecto.",
		Annotations: writes("Pedir un reinicio"),
	}, classWrite, s.requestRestart)

	addTool(s, &sdk.Tool{
		Name: "get_approval",
		Description: "En qué quedó un pedido de este token: pendiente, aprobado, rechazado, vencido o cancelado, " +
			"quién lo decidió y, si se aprobó, el deploy_id para seguirlo con get_deploy.",
		Annotations: readOnly("Estado de un pedido"),
	}, classRead, s.getApproval)
}

type deployInput struct {
	Project string `json:"project" jsonschema:"servidor/nombre, el nombre solo si no hay otro igual, o el ID"`
	Commit  string `json:"commit" jsonschema:"el SHA completo del commit, 40 caracteres en minúscula; tiene que estar en la rama declarada"`
	Reason  string `json:"reason,omitempty" jsonschema:"por qué, en una o dos oraciones, para quien aprueba; hasta 500 caracteres"`
}

type rollbackInput struct {
	Project string `json:"project" jsonschema:"servidor/nombre, el nombre solo si no hay otro igual, o el ID"`
	Reason  string `json:"reason,omitempty" jsonschema:"por qué, en una o dos oraciones, para quien aprueba; hasta 500 caracteres"`
}

type restartInput struct {
	Project string `json:"project" jsonschema:"servidor/nombre, el nombre solo si no hay otro igual, o el ID"`
	Unit    string `json:"unit,omitempty" jsonschema:"el servicio de Compose, su contenedor o la unidad de systemd; si falta, todas las del proyecto"`
	Reason  string `json:"reason,omitempty" jsonschema:"por qué, en una o dos oraciones, para quien aprueba; hasta 500 caracteres"`
}

type approvalInput struct {
	ID string `json:"id" jsonschema:"el approval_id que devolvió deploy, rollback o restart"`
}

// pendingOutput es lo que devuelve una escritura.
type pendingOutput struct {
	Status     string `json:"status" jsonschema:"pending_approval: nada se ejecutó todavía"`
	ApprovalID string `json:"approval_id"`
	Operation  string `json:"operation"`
	Project    string `json:"project"`
	Commit     string `json:"commit,omitempty"`
	Unit       string `json:"unit,omitempty"`
	ExpiresAt  string `json:"expires_at"`
	Message    string `json:"message"`
}

// approvalOutput es un pedido como lo ve get_approval.
type approvalOutput struct {
	ApprovalID   string `json:"approval_id"`
	Operation    string `json:"operation"`
	Project      string `json:"project"`
	Status       string `json:"status" jsonschema:"pending, approved, rejected, expired o cancelled"`
	Commit       string `json:"commit,omitempty"`
	Unit         string `json:"unit,omitempty"`
	RequestedAt  string `json:"requested_at"`
	ExpiresAt    string `json:"expires_at"`
	DecidedAt    string `json:"decided_at,omitempty"`
	DecidedBy    string `json:"decided_by,omitempty"`
	DecisionNote string `json:"decision_note,omitempty"`
	DeployID     string `json:"deploy_id,omitempty" jsonschema:"el deploy que salió de un pedido aprobado: seguilo con get_deploy"`
}

func (s *Server) requestDeploy(ctx context.Context, call *Call, in deployInput) (pendingOutput, error) {
	commit := strings.TrimSpace(in.Commit)
	if !proto.ValidCommit(commit) {
		return pendingOutput{}, errInvalid("commit tiene que ser el SHA completo, 40 caracteres hexadecimales en minúscula (git rev-parse HEAD)")
	}
	project, err := s.writableProject(ctx, call, in.Project, tokens.CapDeploy)
	if err != nil {
		return pendingOutput{}, err
	}
	call.Note("commit", commit)
	return s.request(ctx, call, project, approvals.OpDeploy, commit, "", in.Reason)
}

func (s *Server) requestRollback(ctx context.Context, call *Call, in rollbackInput) (pendingOutput, error) {
	project, err := s.writableProject(ctx, call, in.Project, tokens.CapRollback)
	if err != nil {
		return pendingOutput{}, err
	}
	return s.request(ctx, call, project, approvals.OpRollback, "", "", in.Reason)
}

func (s *Server) requestRestart(ctx context.Context, call *Call, in restartInput) (pendingOutput, error) {
	project, err := s.writableProject(ctx, call, in.Project, tokens.CapRestart)
	if err != nil {
		return pendingOutput{}, err
	}
	unit := ""
	if raw := strings.TrimSpace(in.Unit); raw != "" {
		// Solo unidades del proyecto, como get_logs: el contenedor se pide por
		// su servicio de Compose, que es lo que reinicia el agent.
		detail, err := s.backend.Project(ctx, project.ID)
		if errors.Is(err, ErrNotFound) {
			return pendingOutput{}, errNotFound("no hay un proyecto %q que este token alcance", in.Project)
		}
		if err != nil {
			return pendingOutput{}, err
		}
		target, err := pickUnit(detail, raw)
		if err != nil {
			return pendingOutput{}, err
		}
		unit = target.name
		if target.kind == "container" {
			if target.alias == "" {
				return pendingOutput{}, errInvalid("el contenedor %s no es de un servicio de Compose: reiniciá el proyecto entero", target.name)
			}
			unit = target.alias
		}
		call.Note("unidad", unit)
	}
	return s.request(ctx, call, project, approvals.OpRestart, "", unit, in.Reason)
}

// writableProject resuelve el proyecto con el permiso de escritura. Si el
// token no tiene ese permiso en ningún entorno, lo dice; si lo tiene pero no
// alcanza al proyecto, el proyecto no existe para él.
func (s *Server) writableProject(ctx context.Context, call *Call, ref string, capability tokens.Capability) (ProjectInfo, error) {
	project, err := s.resolveProject(ctx, call, ref, capability)
	if err == nil {
		return project, nil
	}
	if call.Token.Allows(capability, tokens.EnvStaging) || call.Token.Allows(capability, tokens.EnvProduction) {
		return ProjectInfo{}, err
	}
	return ProjectInfo{}, errDenied("este token no tiene permiso de %s", capability)
}

// request guarda el pedido. Lo que se ejecuta sale de estos campos; el
// motivo es texto del LLM y solo se muestra.
func (s *Server) request(ctx context.Context, call *Call, project ProjectInfo, op approvals.Operation, commit, unit, reason string) (pendingOutput, error) {
	a, err := s.backend.RequestApproval(ctx, approvals.Request{
		Operation: op, ServerID: project.ServerID, ProjectID: project.ID, ProjectName: project.Name,
		Environment: project.Environment, Commit: commit, Unit: unit, Reason: reason,
		TokenID: call.Token.ID, TokenName: call.Token.Name, TokenPrefix: call.Token.Prefix, IP: call.IP,
	})
	var pending *approvals.PendingExistsError
	switch {
	case errors.As(err, &pending):
		return pendingOutput{}, &toolError{
			result:  resultConflict,
			message: "ese proyecto ya tiene un pedido esperando (" + pending.ID.String() + "): seguilo con get_approval, o esperá a que lo decidan o venza",
		}
	case errors.Is(err, approvals.ErrInvalid):
		return pendingOutput{}, errInvalid("%s", strings.TrimPrefix(err.Error(), approvals.ErrInvalid.Error()+": "))
	case err != nil:
		return pendingOutput{}, err
	}
	call.Note("pedido", a.ID.String())
	return pendingOutput{
		Status: statusPendingApproval, ApprovalID: a.ID.String(), Operation: string(op), Project: projectRef(project),
		Commit: commit, Unit: unit, ExpiresAt: timestamp(&a.ExpiresAt), Message: pendingMessage,
	}, nil
}

// getApproval devuelve un pedido del mismo token: los de otros tokens no
// existen para él.
func (s *Server) getApproval(ctx context.Context, call *Call, in approvalInput) (approvalOutput, error) {
	id, err := uuid.Parse(strings.TrimSpace(in.ID))
	if err != nil {
		return approvalOutput{}, errInvalid("id tiene que ser el approval_id que devolvió la escritura")
	}
	a, err := s.backend.Approval(ctx, id)
	if errors.Is(err, ErrNotFound) || (err == nil && (a.RequestedByToken == nil || *a.RequestedByToken != call.Token.ID)) {
		return approvalOutput{}, errNotFound("este token no hizo un pedido %s", id)
	}
	if err != nil {
		return approvalOutput{}, err
	}
	call.Target("approval", a.ID.String())
	status := string(a.Status)
	if a.Status == approvals.StatusPending && !s.now().Before(a.ExpiresAt) {
		status = string(approvals.StatusExpired)
	}
	out := approvalOutput{
		ApprovalID: a.ID.String(), Operation: string(a.Operation), Project: a.ServerName + "/" + a.ProjectName,
		Status: status, Commit: a.Commit, Unit: a.Unit, RequestedAt: timestamp(&a.RequestedAt),
		ExpiresAt: timestamp(&a.ExpiresAt), DecidedAt: timestamp(a.DecidedAt), DecidedBy: a.DecidedByName, DecisionNote: a.DecisionNote,
	}
	if a.DeployID != nil {
		out.DeployID = a.DeployID.String()
	}
	return out, nil
}
