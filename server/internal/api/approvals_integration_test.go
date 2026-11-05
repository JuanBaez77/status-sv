//go:build integration

package api_test

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/approvals"
	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/tokens"
)

type approvalBody struct {
	ID            uuid.UUID  `json:"id"`
	Operation     string     `json:"operation"`
	Server        string     `json:"server"`
	Project       string     `json:"project"`
	Commit        string     `json:"commit"`
	Unit          string     `json:"unit"`
	Reason        string     `json:"reason"`
	RequestedBy   string     `json:"requested_by"`
	TokenPrefix   string     `json:"token_prefix"`
	Status        string     `json:"status"`
	DecidedBy     string     `json:"decided_by"`
	DecisionNote  string     `json:"decision_note"`
	DeployID      *uuid.UUID `json:"deploy_id"`
	Repo          string     `json:"repo"`
	RunningCommit string     `json:"running_commit"`
}

type approvalListBody struct {
	Approvals []approvalBody `json:"approvals"`
}

// approvalFixture es un servidor con tienda en producción, su agent
// contestando, una persona con TOTP y un token del MCP con escritura.
type approvalFixture struct {
	*fixture
	ip        string
	cookie    *http.Cookie
	secret    []byte
	agent     *deployAgent
	serverID  uuid.UUID
	projectID uuid.UUID
	token     tokens.Token
	service   *approvals.Service
}

func newApprovalFixture(t *testing.T, ip string, scopes ...string) *approvalFixture {
	t.Helper()
	f := setup(t)
	f.createUser(t)
	cookie, secret := f.loginWithTOTP(t, ip)
	conn, serverID := connectAgent(t, f, "vps-aprobaciones", "203.0.113.120")
	agent := newDeployAgent(t, conn, acceptAll)
	agent.declare(t, dockerProject("tienda", proto.EnvProduction))
	projectID := uuid.MustParse(f.projectID(t, cookie, ip, "tienda"))
	if len(scopes) == 0 {
		scopes = []string{"read:production", "deploy:production", "rollback:production", "restart:production"}
	}
	_, tok := f.mcpToken(t, "claude-code", scopes)
	return &approvalFixture{
		fixture: f, ip: ip, cookie: cookie, secret: secret, agent: agent,
		serverID: serverID, projectID: projectID, token: tok, service: approvals.NewService(f.pool),
	}
}

// ask guarda un pedido como lo guarda el MCP (PR 9).
func (f *approvalFixture) ask(t *testing.T, op approvals.Operation, commit, unit string) approvals.Approval {
	t.Helper()
	a, err := f.service.Create(t.Context(), approvals.Request{
		Operation: op, ServerID: f.serverID, ProjectID: f.projectID, ProjectName: "tienda", Environment: "production",
		Commit: commit, Unit: unit, Reason: "arregla el total del PDF", TokenID: f.token.ID, TokenName: f.token.Name,
		TokenPrefix: f.token.Prefix, IP: "198.51.100.200",
	})
	if err != nil {
		t.Fatalf("guardar el pedido: %v", err)
	}
	return a
}

// code es un código TOTP que todavía no se usó: cada aprobación gasta uno.
func (f *approvalFixture) code(step int64) string {
	return auth.TOTPCode(f.secret, auth.TOTPStep(time.Now())+step)
}

func (f *approvalFixture) approve(t *testing.T, id uuid.UUID, body string) response {
	t.Helper()
	return f.do(t, http.MethodPost, "/v1/approvals/"+id.String()+"/approve", body, f.ip, f.cookie, nil)
}

func TestApproveADeployRequestedByTheMCP(t *testing.T) {
	f := newApprovalFixture(t, "198.51.100.121")
	a := f.ask(t, approvals.OpDeploy, commitB, "")

	pending := decodeJSON[approvalListBody](t, f.do(t, http.MethodGet, "/v1/approvals?status=pending", "", f.ip, f.cookie, nil))
	if len(pending.Approvals) != 1 {
		t.Fatalf("pendientes = %+v", pending)
	}
	got := pending.Approvals[0]
	if got.ID != a.ID || got.Operation != "deploy" || got.Commit != commitB || got.Reason != "arregla el total del PDF" ||
		got.RequestedBy != "claude-code" || got.Repo != "github.com/a/tienda" || got.Status != "pending" || got.Server != "vps-aprobaciones" {
		t.Errorf("pendiente = %+v", got)
	}
	// Sin sesión, nada.
	if resp := f.do(t, http.MethodGet, "/v1/approvals", "", f.ip, nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("sin sesión = %d", resp.StatusCode)
	}

	// Hace falta escribir el nombre y un código nuevo.
	if resp := f.approve(t, a.ID, `{"totp_code":"`+f.code(1)+`"}`); resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(resp.body), "confirmation_required") {
		t.Fatalf("sin el nombre = %d %s", resp.StatusCode, resp.body)
	}
	used := auth.TOTPCode(f.secret, auth.TOTPStep(time.Now()))
	if resp := f.approve(t, a.ID, `{"confirm":"tienda","totp_code":"`+used+`"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("con el código del login = %d %s", resp.StatusCode, resp.body)
	}
	select {
	case cmd := <-f.agent.commands:
		t.Fatalf("sin aprobar, al agent le llegó %+v", cmd)
	default:
	}

	resp := f.approve(t, a.ID, `{"confirm":"tienda","totp_code":"`+f.code(1)+`"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("aprobar = %d %s", resp.StatusCode, resp.body)
	}
	approved := decodeJSON[approvalBody](t, resp)
	if approved.Status != "approved" || approved.DecidedBy != "juan" || approved.DeployID == nil {
		t.Fatalf("aprobado = %+v", approved)
	}

	// Al agent le llega exactamente el commit aprobado.
	envelope := <-f.agent.commands
	cmd, err := proto.Decode[proto.Command](envelope)
	if err != nil || cmd.Operation != proto.OpDeploy || cmd.Deploy.Project != "tienda" || cmd.Deploy.Commit != commitB {
		t.Fatalf("comando = %+v, %v", cmd, err)
	}
	if envelope.ID != *approved.DeployID {
		t.Errorf("el ID del comando no es el del deploy")
	}
	deploy := decodeJSON[deployBody](t, f.do(t, http.MethodGet, "/v1/deploys/"+approved.DeployID.String(), "", f.ip, f.cookie, nil))
	if deploy.Kind != "deploy" || deploy.RequestedBy != "claude-code" || deploy.Status != "pending" {
		t.Errorf("deploy = %+v", deploy)
	}

	if resp := f.approve(t, a.ID, `{"confirm":"tienda","totp_code":"`+f.code(2)+`"}`); resp.StatusCode != http.StatusConflict {
		t.Errorf("aprobar dos veces = %d", resp.StatusCode)
	}

	// La auditoría: quién aprobó, y el deploy que salió con el pedido y el
	// token que lo pidió.
	var approveResult, requestActor, requestDetail string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT result FROM audit_log WHERE action = 'approval.approve' AND actor = 'juan' AND actor_type = 'user' AND target_id = $1 AND result = 'ok'`,
		a.ID.String()).Scan(&approveResult); err != nil {
		t.Errorf("approval.approve: %v", err)
	}
	if err := f.pool.QueryRow(t.Context(),
		`SELECT actor, detail::text FROM audit_log WHERE action = 'deploy.request' AND result = 'accepted'`).Scan(&requestActor, &requestDetail); err != nil {
		t.Fatalf("deploy.request: %v", err)
	}
	if requestActor != "juan" || !strings.Contains(requestDetail, a.ID.String()) || !strings.Contains(requestDetail, "claude-code") || !strings.Contains(requestDetail, commitB) {
		t.Errorf("deploy.request = %s %s", requestActor, requestDetail)
	}
}

func TestApproveARestartOfOneService(t *testing.T) {
	f := newApprovalFixture(t, "198.51.100.122")
	a := f.ask(t, approvals.OpRestart, "", "web")
	resp := f.approve(t, a.ID, `{"confirm":"tienda","totp_code":"`+f.code(1)+`"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("aprobar = %d %s", resp.StatusCode, resp.body)
	}
	cmd, err := proto.Decode[proto.Command](<-f.agent.commands)
	if err != nil || cmd.Operation != proto.OpRestart || cmd.Restart.Project != "tienda" || cmd.Restart.Unit != "web" {
		t.Fatalf("comando = %+v, %v", cmd, err)
	}
	approved := decodeJSON[approvalBody](t, resp)
	deploy := decodeJSON[deployBody](t, f.do(t, http.MethodGet, "/v1/deploys/"+approved.DeployID.String(), "", f.ip, f.cookie, nil))
	if deploy.Kind != "restart" {
		t.Errorf("deploy = %+v", deploy)
	}
}

func TestRejectNeedsNoCode(t *testing.T) {
	f := newApprovalFixture(t, "198.51.100.123")
	a := f.ask(t, approvals.OpRollback, "", "")
	resp := f.do(t, http.MethodPost, "/v1/approvals/"+a.ID.String()+"/reject", `{"note":"todavía no"}`, f.ip, f.cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rechazar = %d %s", resp.StatusCode, resp.body)
	}
	rejected := decodeJSON[approvalBody](t, resp)
	if rejected.Status != "rejected" || rejected.DecisionNote != "todavía no" || rejected.DecidedBy != "juan" {
		t.Errorf("rechazado = %+v", rejected)
	}
	if resp := f.approve(t, a.ID, `{"confirm":"tienda","totp_code":"`+f.code(1)+`"}`); resp.StatusCode != http.StatusConflict {
		t.Errorf("aprobar uno rechazado = %d", resp.StatusCode)
	}
	select {
	case cmd := <-f.agent.commands:
		t.Fatalf("al agent le llegó %+v", cmd)
	default:
	}
}

func TestARequestDiesWithItsToken(t *testing.T) {
	f := newApprovalFixture(t, "198.51.100.124")
	a := f.ask(t, approvals.OpDeploy, commitB, "")
	if _, err := tokens.NewService(f.pool, slog.New(slog.DiscardHandler)).Revoke(t.Context(), f.token.ID, "juan"); err != nil {
		t.Fatal(err)
	}
	resp := f.approve(t, a.ID, `{"confirm":"tienda","totp_code":"`+f.code(1)+`"}`)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(resp.body), "revocado o venció") {
		t.Fatalf("aprobar con el token revocado = %d %s", resp.StatusCode, resp.body)
	}
	got := decodeJSON[approvalBody](t, f.do(t, http.MethodGet, "/v1/approvals/"+a.ID.String(), "", f.ip, f.cookie, nil))
	if got.Status != "cancelled" || got.DecisionNote != "el token que lo pidió fue revocado o venció" {
		t.Errorf("pedido = %+v", got)
	}
	var actor, actorType string
	if err := f.pool.QueryRow(t.Context(), `SELECT actor, actor_type FROM audit_log WHERE action = 'approval.cancel' AND target_id = $1`,
		a.ID.String()).Scan(&actor, &actorType); err != nil || actor != "control-plane" || actorType != "system" {
		t.Errorf("approval.cancel = %s %s, %v", actor, actorType, err)
	}
	select {
	case cmd := <-f.agent.commands:
		t.Fatalf("al agent le llegó %+v", cmd)
	default:
	}
}

func TestARequestNeedsTheScopeStill(t *testing.T) {
	// Un token que solo puede pedir reinicios no puede tener un deploy
	// aprobado, aunque el pedido se hubiera guardado.
	f := newApprovalFixture(t, "198.51.100.125", "read:production", "restart:production")
	a := f.ask(t, approvals.OpDeploy, commitB, "")
	resp := f.approve(t, a.ID, `{"confirm":"tienda","totp_code":"`+f.code(1)+`"}`)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(resp.body), "ya no tiene el permiso") {
		t.Fatalf("aprobar sin el scope = %d %s", resp.StatusCode, resp.body)
	}
}

func TestARequestWaitsForTheAgent(t *testing.T) {
	f := newApprovalFixture(t, "198.51.100.126")
	a := f.ask(t, approvals.OpRestart, "", "")
	_ = f.agent.conn.Close(websocket.StatusNormalClosure, "")
	time.Sleep(200 * time.Millisecond)
	resp := f.approve(t, a.ID, `{"confirm":"tienda","totp_code":"`+f.code(1)+`"}`)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(resp.body), "agent_offline") {
		t.Fatalf("aprobar con el agent desconectado = %d %s", resp.StatusCode, resp.body)
	}
	got := decodeJSON[approvalBody](t, f.do(t, http.MethodGet, "/v1/approvals/"+a.ID.String(), "", f.ip, f.cookie, nil))
	if got.Status != "pending" {
		t.Errorf("con el agent desconectado el pedido sigue pendiente, y está %s", got.Status)
	}
}
