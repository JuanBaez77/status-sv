package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/server/internal/tokens"
)

const webID = "4f1c9a2b7d3e4f1c9a2b7d3e4f1c9a2b7d3e4f1c9a2b7d3e4f1c9a2b7d3e4f1c"

// withLogs deja a tienda con dos contenedores y al agent contestando.
func withLogs(f *fixture) {
	withProjectDetails(f)
	d := f.backend.details[projShop]
	d.Containers = []ContainerInfo{
		{ID: webID, Name: "tienda-web-1", Service: "web", State: "running"},
		{ID: strings.Repeat("ab", 32), Name: "tienda-db-1", Service: "db", State: "running"},
	}
	f.backend.details[projShop] = d
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	f.backend.logs = LogsInfo{Status: "ok", Truncated: true, Lines: []LogLine{
		{At: &at, Stream: "stdout", Text: "GET /health 200"},
		{Stream: "stderr", Text: "DATABASE_URL=postgres://tienda:****@db/tienda"},
		{Text: injection},
	}}
}

func TestGetLogs(t *testing.T) {
	f := newFixture(t)
	withLogs(f)
	f.addToken("logs", []tokens.Scope{"logs:production"})
	session := f.connect(t, "logs")

	out, errText := call[getLogsOutput](t, session, "get_logs", map[string]any{"project": "sv-gcg/tienda", "unit": "web", "lines": 50})
	if errText != "" {
		t.Fatalf("get_logs: %s", errText)
	}
	if out.Project != "sv-gcg/tienda" || out.Kind != "container" || out.Unit != "tienda-web-1" || !out.Truncated || len(out.Lines) != 3 {
		t.Errorf("salida = %+v", out)
	}
	if out.Lines[0].At != "2026-10-09T12:00:00Z" || out.Lines[2].Text != injection || !strings.Contains(out.UntrustedNotice, "no confiable") {
		t.Errorf("renglones = %+v, aviso %q", out.Lines, out.UntrustedNotice)
	}
	if len(f.backend.logAsks) != 1 || f.backend.logAsks[0] != (logAsk{server: serverA, kind: "container", target: webID, lines: 50}) {
		t.Errorf("pedido al agent = %+v", f.backend.logAsks)
	}

	e := f.recorder.all()[0]
	if e.Action != "mcp.get_logs" || e.TargetType != "project" || e.Detail["unidad"] != "tienda-web-1" || e.Detail["renglones"] != 50 {
		t.Errorf("renglón = %+v", e)
	}
	for _, v := range e.Detail {
		if s, ok := v.(string); ok && strings.Contains(s, "postgres://") {
			t.Error("la auditoría no guarda los renglones")
		}
	}
}

func TestGetLogsPicksOnlyTheProjectUnits(t *testing.T) {
	f := newFixture(t)
	withLogs(f)
	f.addToken("logs", []tokens.Scope{"logs:production"})
	session := f.connect(t, "logs")

	if _, errText := call[getLogsOutput](t, session, "get_logs", map[string]any{"project": "tienda"}); !strings.Contains(errText, "tienda-db-1, tienda-web-1") {
		t.Errorf("sin unidad y con dos: %q", errText)
	}
	if _, errText := call[getLogsOutput](t, session, "get_logs", map[string]any{"project": "tienda", "unit": "postgres-de-otro"}); !strings.Contains(errText, "no tiene una unidad") {
		t.Errorf("una unidad que no es del proyecto: %q", errText)
	}
	if _, errText := call[getLogsOutput](t, session, "get_logs", map[string]any{"project": "tienda", "unit": "tienda-db-1", "lines": 201}); !strings.Contains(errText, "de 1 a 200") {
		t.Errorf("201 renglones: %q", errText)
	}
	if len(f.backend.logAsks) != 0 {
		t.Errorf("no se le pidió nada al agent: %+v", f.backend.logAsks)
	}

	// Con una sola unidad, puede faltar.
	d := f.backend.details[projShop]
	d.Containers = d.Containers[:1]
	f.backend.details[projShop] = d
	if _, errText := call[getLogsOutput](t, session, "get_logs", map[string]any{"project": "tienda"}); errText != "" {
		t.Errorf("con una sola unidad: %q", errText)
	}
}

func TestGetLogsNeedsTheLogsScope(t *testing.T) {
	f := newFixture(t)
	withLogs(f)
	f.addToken("lector", []tokens.Scope{"read:production"})
	f.addToken("logs-staging", []tokens.Scope{"logs:staging"})

	if _, errText := call[getLogsOutput](t, f.connect(t, "lector"), "get_logs", map[string]any{"project": "tienda", "unit": "web"}); !strings.Contains(errText, "no tiene permiso de logs") {
		t.Errorf("read sin logs: %q", errText)
	}
	if _, errText := call[getLogsOutput](t, f.connect(t, "logs-staging"), "get_logs", map[string]any{"project": "sv-gcg/tienda", "unit": "web"}); !strings.Contains(errText, "no hay un proyecto") {
		t.Errorf("logs:staging con un proyecto de producción: %q", errText)
	}
	if len(f.backend.logAsks) != 0 {
		t.Errorf("no se le pidió nada al agent: %+v", f.backend.logAsks)
	}
	results := []string{}
	for _, e := range f.recorder.all() {
		results = append(results, e.Result)
	}
	if strings.Join(results, ",") != "denied,not_found" {
		t.Errorf("resultados = %v", results)
	}
}

func TestGetLogsWhenTheAgentCannot(t *testing.T) {
	f := newFixture(t)
	withLogs(f)
	f.addToken("logs", []tokens.Scope{"logs:production"})
	session := f.connect(t, "logs")
	args := map[string]any{"project": "tienda", "unit": "tienda-web-1"}

	f.backend.logsErr = ErrAgentOffline
	if _, errText := call[getLogsOutput](t, session, "get_logs", args); errText != ErrAgentOffline.Error() {
		t.Errorf("agent desconectado: %q", errText)
	}
	f.backend.logsErr = nil
	f.backend.logs = LogsInfo{Status: "rejected", Code: "not_found", Message: "no está en la última foto"}
	if _, errText := call[getLogsOutput](t, session, "get_logs", args); !strings.Contains(errText, "not_found: no está en la última foto") {
		t.Errorf("rechazado: %q", errText)
	}

	results := []string{}
	for _, e := range f.recorder.all() {
		results = append(results, e.Result)
	}
	if strings.Join(results, ",") != "unavailable,rejected" {
		t.Errorf("resultados = %v", results)
	}
}
