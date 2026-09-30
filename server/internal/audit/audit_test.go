package audit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/server/internal/store"
)

type fakeDB struct{ rows []store.InsertAuditParams }

func (f *fakeDB) InsertAudit(_ context.Context, arg store.InsertAuditParams) error {
	f.rows = append(f.rows, arg)
	return nil
}

func TestRecord(t *testing.T) {
	db := &fakeDB{}
	r := NewRecorder(db)
	r.now = func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.FixedZone("AR", -3*3600)) }

	err := r.Record(context.Background(), Entry{
		Actor: "juan", ActorType: ActorUser, Action: ActionDeployRequest, TargetType: "project",
		TargetID: "0192-abc", Result: "accepted", IP: "203.0.113.7", Detail: map[string]any{"commit": "abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	row := db.rows[0]
	if row.Actor != "juan" || row.ActorType != "user" || row.Result != "accepted" || row.At.Location() != time.UTC {
		t.Errorf("renglón = %+v", row)
	}
	var detail map[string]string
	if err := json.Unmarshal(row.Detail, &detail); err != nil || detail["commit"] != "abc" {
		t.Errorf("detalle = %s", row.Detail)
	}
}

func TestRecordFitsTheTable(t *testing.T) {
	db := &fakeDB{}
	r := NewRecorder(db)
	err := r.Record(context.Background(), Entry{
		Actor: strings.Repeat("ñ", 100), ActorType: ActorAgent, Action: "", TargetType: "server",
		TargetID: "x", Result: "ok", Detail: map[string]any{"mucho": strings.Repeat("x", maxDetail)},
	})
	if err != nil {
		t.Fatal(err)
	}
	row := db.rows[0]
	if len(row.Actor) > maxActor || !strings.HasPrefix(row.Actor, "ñ") || strings.ContainsRune(row.Actor, '\uFFFD') {
		t.Errorf("actor = %q (%d bytes)", row.Actor, len(row.Actor))
	}
	if row.Action != "desconocida" {
		t.Errorf("acción vacía = %q", row.Action)
	}
	if string(row.Detail) != `{"truncated":true}` {
		t.Errorf("detalle enorme = %s", row.Detail)
	}
	if string(db.rows[0].Detail) == "" {
		t.Error("detalle vacío")
	}
}

func TestRecordRejectsUnknownActorType(t *testing.T) {
	if err := NewRecorder(&fakeDB{}).Record(context.Background(), Entry{ActorType: "robot"}); err == nil {
		t.Fatal("aceptó un tipo de actor desconocido")
	}
}
