package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/server/internal/approvals"
	"github.com/JuanBaez77/status-sv/server/internal/tokens"
)

const sha = "4f1c9a2b7d3e4f1c9a2b7d3e4f1c9a2b7d3e4f1c"

func TestDeployOnlyAsksForAnApproval(t *testing.T) {
	f := newFixture(t)
	f.addToken("escritor", []tokens.Scope{"read:production", "deploy:production"})
	session := f.connect(t, "escritor")

	out, errText := call[pendingOutput](t, session, "deploy", map[string]any{
		"project": "sv-gcg/tienda", "commit": sha, "reason": "arregla el total",
	})
	if errText != "" {
		t.Fatalf("deploy: %s", errText)
	}
	if out.Status != "pending_approval" || out.Operation != "deploy" || out.Project != "sv-gcg/tienda" || out.Commit != sha ||
		out.ApprovalID == "" || !strings.Contains(out.Message, "Nada se ejecutó") || out.ExpiresAt == "" {
		t.Errorf("salida = %+v", out)
	}
	if len(f.backend.requests) != 1 {
		t.Fatalf("pedidos = %+v", f.backend.requests)
	}
	req := f.backend.requests[0]
	if req.Operation != approvals.OpDeploy || req.ProjectID != projShop || req.ServerID != serverA || req.Environment != "production" ||
		req.Commit != sha || req.Reason != "arregla el total" || req.TokenName != "escritor" || req.IP != "203.0.113.7" {
		t.Errorf("pedido = %+v", req)
	}
	e := f.recorder.all()[0]
	if e.Action != "mcp.deploy" || e.Result != "ok" || e.Detail["pedido"] != out.ApprovalID || e.Detail["commit"] != sha {
		t.Errorf("renglón = %+v", e)
	}
	// Con deploy no alcanza para pedir otra cosa.
	if _, errText := call[pendingOutput](t, session, "rollback", map[string]any{"project": "tienda"}); !strings.Contains(errText, "no tiene permiso de rollback") {
		t.Errorf("rollback con deploy: %q", errText)
	}
}

func TestWritesNeedTheirScope(t *testing.T) {
	f := newFixture(t)
	f.addToken("lector", []tokens.Scope{"read:production"})
	f.addToken("staging", []tokens.Scope{"read:production", "deploy:staging"})
	f.addToken("reinicios", []tokens.Scope{"read:production", "restart:production"})

	if _, errText := call[pendingOutput](t, f.connect(t, "lector"), "deploy", map[string]any{"project": "tienda", "commit": sha}); !strings.Contains(errText, "no tiene permiso de deploy") {
		t.Errorf("sin deploy: %q", errText)
	}
	if _, errText := call[pendingOutput](t, f.connect(t, "staging"), "deploy", map[string]any{"project": "sv-gcg/tienda", "commit": sha}); !strings.Contains(errText, "no hay un proyecto") {
		t.Errorf("deploy:staging sobre producción: %q", errText)
	}
	reinicios := f.connect(t, "reinicios")
	if _, errText := call[pendingOutput](t, reinicios, "rollback", map[string]any{"project": "tienda"}); !strings.Contains(errText, "no tiene permiso de rollback") {
		t.Errorf("rollback con restart: %q", errText)
	}
	if len(f.backend.requests) != 0 {
		t.Errorf("no se guardó ningún pedido: %+v", f.backend.requests)
	}
}

func TestDeployNeedsTheWholeCommit(t *testing.T) {
	f := newFixture(t)
	f.addToken("escritor", []tokens.Scope{"deploy:production"})
	session := f.connect(t, "escritor")
	for _, commit := range []string{"", "main", sha[:12], strings.ToUpper(sha), "HEAD~1"} {
		if _, errText := call[pendingOutput](t, session, "deploy", map[string]any{"project": "tienda", "commit": commit}); !strings.Contains(errText, "SHA completo") {
			t.Errorf("commit %q: %q", commit, errText)
		}
	}
	if len(f.backend.requests) != 0 {
		t.Errorf("no se guardó ningún pedido: %+v", f.backend.requests)
	}
}

func TestOnePendingRequestPerProject(t *testing.T) {
	f := newFixture(t)
	f.addToken("escritor", []tokens.Scope{"deploy:production", "rollback:production"})
	session := f.connect(t, "escritor")
	first, _ := call[pendingOutput](t, session, "deploy", map[string]any{"project": "tienda", "commit": sha})
	_, errText := call[pendingOutput](t, session, "rollback", map[string]any{"project": "tienda"})
	if !strings.Contains(errText, "ya tiene un pedido esperando ("+first.ApprovalID+")") {
		t.Errorf("segundo pedido: %q", errText)
	}
	if e := f.recorder.all()[1]; e.Action != "mcp.rollback" || e.Result != "conflict" {
		t.Errorf("renglón = %+v", e)
	}
}

func TestRestartPicksOnlyTheProjectUnits(t *testing.T) {
	f := newFixture(t)
	withLogs(f)
	f.addToken("reinicios", []tokens.Scope{"restart:production"})
	session := f.connect(t, "reinicios")

	// El contenedor se pide por su servicio de Compose, que es lo que se
	// reinicia; uno ajeno no se guarda.
	if _, errText := call[pendingOutput](t, session, "restart", map[string]any{"project": "tienda", "unit": "postgres-de-otro"}); !strings.Contains(errText, "no tiene una unidad") {
		t.Errorf("unidad ajena: %q", errText)
	}
	out, errText := call[pendingOutput](t, session, "restart", map[string]any{"project": "tienda", "unit": "tienda-web-1"})
	if errText != "" || out.Unit != "web" {
		t.Fatalf("por el contenedor: %+v, %q", out, errText)
	}
	if len(f.backend.requests) != 1 || f.backend.requests[0].Unit != "web" || f.backend.requests[0].Operation != approvals.OpRestart {
		t.Errorf("pedidos = %+v", f.backend.requests)
	}
}

func TestRestartOfTheWholeProject(t *testing.T) {
	f := newFixture(t)
	f.addToken("reinicios", []tokens.Scope{"restart:production"})
	out, errText := call[pendingOutput](t, f.connect(t, "reinicios"), "restart", map[string]any{"project": "cristales/cotizador"})
	if errText != "" || out.Unit != "" || out.Project != "cristales/cotizador" {
		t.Fatalf("reinicio = %+v, %q", out, errText)
	}
}

func TestGetApprovalShowsOnlyTheTokensOwnRequests(t *testing.T) {
	f := newFixture(t)
	f.addToken("escritor", []tokens.Scope{"deploy:production"})
	f.addToken("otro", []tokens.Scope{"deploy:production"})
	session := f.connect(t, "escritor")
	pending, _ := call[pendingOutput](t, session, "deploy", map[string]any{"project": "tienda", "commit": sha})

	got, errText := call[approvalOutput](t, session, "get_approval", map[string]any{"id": pending.ApprovalID})
	if errText != "" || got.Status != "pending" || got.Project != "sv-gcg/tienda" || got.Commit != sha || got.DeployID != "" {
		t.Fatalf("pendiente = %+v, %q", got, errText)
	}

	// Aprobado en el panel: ahora tiene su deploy.
	id := uuid.MustParse(pending.ApprovalID)
	a := f.backend.approvals[id]
	deployID, decided := uuid.Must(uuid.NewV7()), time.Now().UTC()
	a.Status, a.DecidedByName, a.DecidedAt, a.DeployID = approvals.StatusApproved, "juan", &decided, &deployID
	f.backend.approvals[id] = a
	got, _ = call[approvalOutput](t, session, "get_approval", map[string]any{"id": pending.ApprovalID})
	if got.Status != "approved" || got.DecidedBy != "juan" || got.DeployID != deployID.String() || got.DecidedAt == "" {
		t.Errorf("aprobado = %+v", got)
	}

	if _, errText := call[approvalOutput](t, f.connect(t, "otro"), "get_approval", map[string]any{"id": pending.ApprovalID}); !strings.Contains(errText, "no hizo un pedido") {
		t.Errorf("el pedido de otro token: %q", errText)
	}
	if _, errText := call[approvalOutput](t, session, "get_approval", map[string]any{"id": "x"}); !strings.Contains(errText, "approval_id") {
		t.Errorf("un ID inválido: %q", errText)
	}

	// Uno pendiente cuyo plazo pasó se muestra vencido.
	a.Status, a.DeployID, a.DecidedAt, a.ExpiresAt = approvals.StatusPending, nil, nil, time.Now().Add(-time.Minute)
	f.backend.approvals[id] = a
	if got, _ := call[approvalOutput](t, session, "get_approval", map[string]any{"id": pending.ApprovalID}); got.Status != "expired" {
		t.Errorf("vencido = %+v", got)
	}
}

func TestWritesHaveTheirOwnLimit(t *testing.T) {
	f := newFixture(t)
	f.addToken("escritor", []tokens.Scope{"read:production", "restart:production"})
	session := f.connect(t, "escritor")
	// Cada pedido se guarda en un proyecto distinto para que no choque con
	// el que espera: lo que frena es el cupo.
	f.backend.pendingFor = map[uuid.UUID]uuid.UUID{}
	for i := range writesBurst {
		f.backend.pendingFor = map[uuid.UUID]uuid.UUID{}
		if _, errText := call[pendingOutput](t, session, "restart", map[string]any{"project": "tienda"}); errText != "" {
			t.Fatalf("pedido %d: %q", i+1, errText)
		}
	}
	f.backend.pendingFor = map[uuid.UUID]uuid.UUID{}
	if _, errText := call[pendingOutput](t, session, "restart", map[string]any{"project": "tienda"}); !strings.Contains(errText, "demasiadas llamadas") {
		t.Errorf("pedido %d: %q", writesBurst+1, errText)
	}
	// Las lecturas siguen andando.
	if _, errText := call[listProjectsOutput](t, session, "list_projects", map[string]any{}); errText != "" {
		t.Errorf("una lectura después del cupo de escrituras: %q", errText)
	}
}
