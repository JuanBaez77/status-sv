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
	ActionDeployFinish    = "deploy.finish"
	ActionLogsRequest     = "logs.request"
)

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
		Actor:      fit(e.Actor, maxActor, "desconocido"),
		ActorType:  string(e.ActorType),
		Action:     fit(e.Action, maxAction, "desconocida"),
		TargetType: fit(e.TargetType, maxTargetType, "desconocido"),
		TargetID:   fit(e.TargetID, maxTargetID, "desconocido"),
		Result:     fit(e.Result, maxResult, "desconocido"),
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
