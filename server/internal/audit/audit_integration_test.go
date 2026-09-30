//go:build integration

package audit

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/server/internal/store"
	"github.com/JuanBaez77/status-sv/server/internal/testdb"
)

// audit_log es solo de inserción: la base rechaza cambiar o borrar renglones,
// también al dueño de la tabla.
func TestAuditLogIsInsertOnly(t *testing.T) {
	pool := testdb.Setup(t)
	r := NewRecorder(store.New(pool))
	target := uuid.Must(uuid.NewV7()).String()
	if err := r.Record(t.Context(), Entry{
		Actor: "juan", ActorType: ActorUser, Action: ActionDeployRequest, TargetType: "project", TargetID: target, Result: "accepted",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_log WHERE target_id = $1", target).Scan(&n); err != nil || n != 1 {
		t.Fatalf("renglones = %d, %v", n, err)
	}

	for name, sql := range map[string]string{
		"UPDATE":   "UPDATE audit_log SET result = 'otra cosa' WHERE target_id = $1",
		"DELETE":   "DELETE FROM audit_log WHERE target_id = $1",
		"TRUNCATE": "TRUNCATE audit_log",
	} {
		args := []any{target}
		if name == "TRUNCATE" {
			args = nil
		}
		_, err := pool.Exec(t.Context(), sql, args...)
		if err == nil {
			t.Errorf("%s sobre audit_log se aceptó", name)
			continue
		}
		// O lo frena el permiso revocado o el trigger; los dos son la base
		// negándose.
		if !strings.Contains(err.Error(), "permission denied") && !strings.Contains(err.Error(), "solo de inserción") {
			t.Errorf("%s falló por otra cosa: %v", name, err)
		}
	}
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_log WHERE target_id = $1 AND result = 'accepted'", target).Scan(&n); err != nil || n != 1 {
		t.Fatalf("el renglón cambió: %d, %v", n, err)
	}
}

func TestAuditLogRejectsBadRows(t *testing.T) {
	pool := testdb.Setup(t)
	q := store.New(pool)
	base := store.InsertAuditParams{
		Actor: "juan", ActorType: "user", Action: "x", TargetType: "project", TargetID: "y", Result: "ok", Detail: []byte(`{}`),
	}
	bad := map[string]func(p *store.InsertAuditParams){
		"tipo de actor":     func(p *store.InsertAuditParams) { p.ActorType = "robot" },
		"detalle no objeto": func(p *store.InsertAuditParams) { p.Detail = []byte(`[1]`) },
		"actor vacío":       func(p *store.InsertAuditParams) { p.Actor = "" },
	}
	for name, mutate := range bad {
		p := base
		p.ID = uuid.Must(uuid.NewV7())
		mutate(&p)
		if err := q.InsertAudit(t.Context(), p); err == nil {
			t.Errorf("%s: se aceptó", name)
		}
	}
}
