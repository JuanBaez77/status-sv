// Package audit escribe la auditoría: quién hizo qué, sobre qué y cómo salió
// (principio 5 del diseño, ADR-017).
//
// audit_log es solo de inserción: la base rechaza UPDATE, DELETE y TRUNCATE
// con un trigger, y este paquete no tiene otra operación que agregar.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// ActorType es quién actuó.
type ActorType string

// Tipos de actor.
const (
	ActorUser   ActorType = "user"
	ActorLLM    ActorType = "llm"
	ActorAgent  ActorType = "agent"
	ActorSystem ActorType = "system"
)

// Acciones que se auditan hoy.
const (
	ActionDeployRequest   = "deploy.request"
	ActionRollbackRequest = "rollback.request"
	// ActionBootstrapRequest es el pedido del primer deploy de un proyecto
	// (ADR-021).
	ActionBootstrapRequest = "bootstrap.request"
	// ActionRestartRequest es el pedido de un reinicio, que sale de una
	// aprobación (ADR-026).
	ActionRestartRequest = "restart.request"
	ActionDeployFinish   = "deploy.finish"
	ActionLogsRequest    = "logs.request"

	// Las del alta de proyectos (ADR-022): la propuesta, su retiro, y que se
	// aceptó en el servidor (la informa el agent).
	ActionProjectPropose  = "project.propose"
	ActionProjectWithdraw = "project.withdraw"
	ActionProjectAccept   = "project.accept"

	// Las de acceso (ADR-020).
	ActionLogin             = "auth.login"
	ActionTOTPVerify        = "auth.totp_verify"
	ActionTOTPEnroll        = "auth.totp_enroll"
	ActionRecoveryCode      = "auth.recovery_code"
	ActionLogout            = "auth.logout"
	ActionAgentEnroll       = "agent.enroll"
	ActionUserCreate        = "user.create"
	ActionUserPassword      = "user.password"
	ActionUserTOTPReset     = "user.totp_reset"
	ActionEnrollTokenCreate = "enrollment_token.create"

	// Las de los tokens del MCP (ADR-025).
	ActionTokenCreate = "token.create"
	ActionTokenRevoke = "token.revoke"
	// ActionMCPAuth es un pedido al MCP con un token que no sirve. Cada
	// llamada a una herramienta se anota como "mcp.<herramienta>" (ADR-024).
	ActionMCPAuth = "mcp.auth"
	// Los pedidos de escritura del MCP (ADR-026). Los pide un token, como
	// llm; los aprueba o los rechaza una persona; los vence o los cancela el
	// sistema.
	ActionApprovalRequest = "approval.request"
	ActionApprovalApprove = "approval.approve"
	ActionApprovalReject  = "approval.reject"
	ActionApprovalExpire  = "approval.expire"
	ActionApprovalCancel  = "approval.cancel"
)

// Resultados de las acciones de acceso. Las de deploy usan el estado del
// deploy o el motivo del rechazo.
const (
	ResultOK     = "ok"
	ResultFailed = "failed"
	// ResultBlocked es un pedido frenado por el límite de intentos.
	ResultBlocked = "blocked"
	// ResultRejected es un enrolamiento con un token que no sirve.
	ResultRejected = "rejected"
)

// Unknown es el actor o el objetivo cuando no se sabe quién es: un login con
// un nombre que no existe, un enrolamiento con un token inválido. Lo que se
// escribió no se guarda (ADR-020).
const Unknown = "desconocido"

// ActorCLI es el actor de los subcomandos que alguien corre en el servidor.
const ActorCLI = "cli"

// Límites de la tabla (migración 00009).
const (
	maxActor      = 128
	maxAction     = 64
	maxTargetType = 32
	maxTargetID   = 128
	maxResult     = 32
	maxIP         = 64
	maxDetail     = 4096
)

// Entry es un renglón de auditoría.
type Entry struct {
	Actor      string
	ActorType  ActorType
	Action     string
	TargetType string
	TargetID   string
	Result     string
	IP         string
	// Detail es un objeto chico con lo que ayude a entender el renglón
	// (commit, código de error). Nunca secretos.
	Detail map[string]any
}

// inserter es lo que se usa de la base.
type inserter interface {
	InsertAudit(ctx context.Context, arg store.InsertAuditParams) error
}

// Recorder escribe renglones de auditoría.
type Recorder struct {
	db  inserter
	now func() time.Time
}

// NewRecorder arma un Recorder sobre la base.
func NewRecorder(db inserter) *Recorder {
	return &Recorder{db: db, now: time.Now}
}

// Record agrega un renglón. Los textos se recortan a lo que acepta la tabla:
// perder la cola de un nombre larguísimo es mejor que perder el renglón.
func (r *Recorder) Record(ctx context.Context, e Entry) error {
	switch e.ActorType {
	case ActorUser, ActorLLM, ActorAgent, ActorSystem:
	default:
		return fmt.Errorf("tipo de actor desconocido %q", e.ActorType)
	}
	detail := []byte("{}")
	if len(e.Detail) > 0 {
		raw, err := json.Marshal(e.Detail)
		if err != nil {
			return fmt.Errorf("serializar el detalle: %w", err)
		}
		if len(raw) <= maxDetail {
			detail = raw
		} else {
			detail = []byte(`{"truncated":true}`)
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	err = r.db.InsertAudit(ctx, store.InsertAuditParams{
		ID:         id,
		At:         r.now().UTC(),
		Actor:      fit(e.Actor, maxActor, Unknown),
		ActorType:  string(e.ActorType),
		Action:     fit(e.Action, maxAction, "desconocida"),
		TargetType: fit(e.TargetType, maxTargetType, Unknown),
		TargetID:   fit(e.TargetID, maxTargetID, Unknown),
		Result:     fit(e.Result, maxResult, Unknown),
		Ip:         fit(e.IP, maxIP, ""),
		Detail:     detail,
	})
	if err != nil {
		return fmt.Errorf("escribir la auditoría: %w", err)
	}
	return nil
}

// fit recorta un texto a max bytes sin partir un carácter, y usa empty si
// queda vacío.
func fit(s string, maxLen int, empty string) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, "?"))
	if len(s) > maxLen {
		// Cortar en medio de un carácter deja bytes sueltos al final, que
		// ToValidUTF8 saca.
		s = strings.ToValidUTF8(s[:maxLen], "")
	}
	if s == "" {
		return empty
	}
	return s
}
