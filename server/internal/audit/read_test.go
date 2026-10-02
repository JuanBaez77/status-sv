package audit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// fakeLister devuelve hasta MaxRows renglones de los que tiene y guarda lo
// que le pidieron.
type fakeLister struct {
	rows []store.AuditLog
	got  store.ListAuditParams
}

func (f *fakeLister) ListAudit(_ context.Context, arg store.ListAuditParams) ([]store.AuditLog, error) {
	f.got = arg
	n := min(len(f.rows), int(arg.MaxRows))
	return f.rows[:n], nil
}

func auditRows(n int) []store.AuditLog {
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	rows := make([]store.AuditLog, n)
	for i := range rows {
		rows[i] = store.AuditLog{
			ID: uuid.Must(uuid.NewV7()), At: base.Add(-time.Duration(i) * time.Minute),
			Actor: "juan", ActorType: "user", Action: ActionLogin, TargetType: "user", TargetID: "juan",
			Result: ResultOK, Detail: []byte(`{}`),
		}
	}
	return rows
}

func TestFilterValidate(t *testing.T) {
	now := time.Now()
	bad := map[string]Filter{
		"tipo de actor":        {ActorType: "robot"},
		"acción sin punto":     {Action: "login"},
		"acción con comillas":  {Action: "auth.login' OR 1=1"},
		"acción larguísima":    {Action: strings.Repeat("a", 60) + ".login"},
		"tipo de objetivo":     {TargetType: "Project"},
		"resultado":            {Result: "ok ok"},
		"objetivo con control": {TargetID: "abc\x00"},
		"objetivo larguísimo":  {TargetID: strings.Repeat("a", 129)},
		"rango al revés":       {From: now, To: now.Add(-time.Hour)},
		"página enorme":        {Limit: MaxPage + 1},
		"página negativa":      {Limit: -1},
	}
	for name, f := range bad {
		if err := f.Validate(); !errors.Is(err, ErrInvalidFilter) {
			t.Errorf("%s: Validate = %v, se esperaba ErrInvalidFilter", name, err)
		}
	}

	good := []Filter{
		{},
		{ActorType: "agent", Action: ActionDeployFinish, TargetType: "project", TargetID: "0199-abc", Result: "rolled_back"},
		{From: now.Add(-time.Hour), To: now, Limit: MaxPage},
		{Action: ActionEnrollTokenCreate},
	}
	for _, f := range good {
		if err := f.Validate(); err != nil {
			t.Errorf("%+v: Validate = %v", f, err)
		}
	}
}

func TestListPagesWithACursor(t *testing.T) {
	rows := auditRows(5)
	db := &fakeLister{rows: rows}
	r := NewReader(db)

	page, err := r.List(t.Context(), Filter{Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Se pide uno de más para saber si hay otra página.
	if db.got.MaxRows != 3 {
		t.Errorf("MaxRows = %d, se esperaba 3", db.got.MaxRows)
	}
	if len(page.Rows) != 2 || page.Rows[1].ID != rows[1].ID {
		t.Fatalf("la página trae %d renglones", len(page.Rows))
	}
	if page.Next == "" {
		t.Fatal("hay más renglones y no vino el cursor")
	}

	// El cursor es el último renglón que se entregó, no el de más.
	if _, err := r.List(t.Context(), Filter{Limit: 2, Before: page.Next}); err != nil {
		t.Fatalf("List con cursor: %v", err)
	}
	if db.got.BeforeAt == nil || !db.got.BeforeAt.Equal(rows[1].At) || db.got.BeforeID == nil || *db.got.BeforeID != rows[1].ID {
		t.Errorf("el cursor no apunta al último renglón entregado: %v %v", db.got.BeforeAt, db.got.BeforeID)
	}
}

func TestListLastPageHasNoCursor(t *testing.T) {
	db := &fakeLister{rows: auditRows(2)}
	page, err := NewReader(db).List(t.Context(), Filter{Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Rows) != 2 || page.Next != "" {
		t.Errorf("renglones = %d, cursor = %q", len(page.Rows), page.Next)
	}
	if db.got.ActorType != nil || db.got.Action != nil || db.got.FromAt != nil || db.got.BeforeAt != nil {
		t.Errorf("un filtro vacío llegó a la consulta: %+v", db.got)
	}

	// Sin tamaño de página, el de siempre.
	if _, err := NewReader(db).List(t.Context(), Filter{}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if db.got.MaxRows != DefaultPage+1 {
		t.Errorf("MaxRows = %d, se esperaba %d", db.got.MaxRows, DefaultPage+1)
	}
}

func TestListRejectsBadCursors(t *testing.T) {
	r := NewReader(&fakeLister{})
	for _, cursor := range []string{"abc", "123", "123_no-es-un-uuid", "-5_" + uuid.NewString(), "x_" + uuid.NewString()} {
		if _, err := r.List(t.Context(), Filter{Before: cursor}); !errors.Is(err, ErrInvalidFilter) {
			t.Errorf("cursor %q: List = %v, se esperaba ErrInvalidFilter", cursor, err)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 2, 6, 10, 3, 123456000, time.UTC)
	id := uuid.Must(uuid.NewV7())
	gotAt, gotID, err := parseCursor(cursor(at, id))
	if err != nil || !gotAt.Equal(at) || gotID != id {
		t.Errorf("parseCursor = %v, %v, %v", gotAt, gotID, err)
	}
}
