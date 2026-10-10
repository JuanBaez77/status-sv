//go:build integration

package api_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/deploys"
	"github.com/JuanBaez77/status-sv/server/internal/tokens"
)

// mcpToken crea un token del MCP directo en la base y devuelve su valor.
func (f *fixture) mcpToken(t *testing.T, name string, scopes []string, projects ...uuid.UUID) (string, tokens.Token) {
	t.Helper()
	service := tokens.NewService(f.pool, slog.New(slog.DiscardHandler))
	secret, tok, err := service.Create(t.Context(), tokens.NewToken{
		Name: name, Scopes: scopes, ProjectIDs: projects, TTL: time.Hour, CreatedByName: "juan",
	})
	if err != nil {
		t.Fatalf("crear el token: %v", err)
	}
	return secret.Reveal(), tok
}

type headerTransport struct {
	header map[string]string
}

func (h headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h.header {
		req.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// mcpSession conecta un cliente MCP de verdad con el token, desde una IP.
func (f *fixture) mcpSession(t *testing.T, token, ip string) (*sdk.ClientSession, error) {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &sdk.StreamableClientTransport{
		Endpoint: f.srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: headerTransport{header: map[string]string{
			"Authorization":   "Bearer " + token,
			"X-Forwarded-For": ip,
		}}},
		MaxRetries: -1,
	}, nil)
	if err == nil {
		t.Cleanup(func() { _ = session.Close() })
	}
	return session, err
}

func TestMCPListServersWithAToken(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.80"
	projectID := f.declaredProject(t, "vps-mcp", "tienda", "production")
	f.declaredProject(t, "vps-otro", "otro", "production")
	value, tok := f.mcpToken(t, "claude-code", []string{"read:production"}, projectID)

	session, err := f.mcpSession(t, value, ip)
	if err != nil {
		t.Fatalf("conectar: %v", err)
	}
	res, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: "list_servers", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("list_servers = %+v, %v", res, err)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out struct {
		Servers []struct {
			Name     string   `json:"name"`
			Status   string   `json:"status"`
			Projects []string `json:"projects"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Servers) != 1 || out.Servers[0].Name != "vps-mcp" || strings.Join(out.Servers[0].Projects, ",") != "tienda" {
		t.Errorf("servidores = %+v; el token con lista ve solo el servidor de su proyecto", out.Servers)
	}

	entries := f.auditFrom(t, ip)
	if len(entries) != 1 {
		t.Fatalf("auditoría = %+v", entries)
	}
	if e := entries[0]; e.Actor != "claude-code" || e.ActorType != "llm" || e.Action != "mcp.list_servers" ||
		e.Result != "ok" || e.Detail["token"] != tok.Prefix {
		t.Errorf("renglón = %+v", e)
	}

	// El uso quedó anotado, con la IP.
	got, err := tokens.NewService(f.pool, slog.New(slog.DiscardHandler)).Get(t.Context(), tok.ID)
	if err != nil || got.LastUsedAt == nil || got.LastUsedIP != ip {
		t.Errorf("último uso = %v desde %q (%v)", got.LastUsedAt, got.LastUsedIP, err)
	}
}

func TestMCPRejectsWithoutAValidToken(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.81"
	value, tok := f.mcpToken(t, "revocable", []string{"read:staging"})

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"1"}}}`
	accept := map[string]string{"Accept": "application/json, text/event-stream"}
	with := func(extra map[string]string) map[string]string {
		h := map[string]string{}
		for k, v := range accept {
			h[k] = v
		}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}

	if resp := f.do(t, http.MethodPost, "/mcp", initialize, ip, nil, accept); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("sin token = %d", resp.StatusCode)
	}
	if resp := f.do(t, http.MethodPost, "/mcp", initialize, ip, nil, with(map[string]string{"Authorization": "Bearer svmcp_inventado"})); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("token inventado = %d", resp.StatusCode)
	}
	// Un navegador no entra, ni con un token bueno.
	if resp := f.do(t, http.MethodPost, "/mcp", initialize, ip, nil, with(map[string]string{
		"Authorization": "Bearer " + value, "Origin": "https://evil.example",
	})); resp.StatusCode != http.StatusForbidden {
		t.Errorf("con Origin = %d", resp.StatusCode)
	}
	// Sin estado: no hay canal de eventos.
	if resp := f.do(t, http.MethodGet, "/mcp", "", ip, nil, with(map[string]string{"Authorization": "Bearer " + value})); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d", resp.StatusCode)
	}
	if resp := f.do(t, http.MethodPost, "/mcp", initialize, ip, nil, with(map[string]string{"Authorization": "Bearer " + value})); resp.StatusCode != http.StatusOK {
		t.Errorf("con el token = %d: %s", resp.StatusCode, resp.body)
	}

	// La sesión del panel no sirve para el MCP.
	f.createUser(t)
	cookie, _ := f.loginWithTOTP(t, ip)
	if resp := f.do(t, http.MethodPost, "/mcp", initialize, ip, cookie, accept); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("con la cookie del panel = %d", resp.StatusCode)
	}

	if _, err := tokens.NewService(f.pool, slog.New(slog.DiscardHandler)).Revoke(t.Context(), tok.ID, "juan"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mcpSession(t, value, ip); err == nil {
		t.Error("un token revocado no puede conectarse")
	}

	var rejected int
	for _, e := range f.auditFrom(t, ip) {
		if e.Action == "mcp.auth" {
			rejected++
			if e.Result != "rejected" || e.ActorType != "llm" {
				t.Errorf("renglón = %+v", e)
			}
		}
	}
	if rejected != 1 {
		t.Errorf("los tokens que no sirven se anotan una vez cada 15 minutos por IP, y se anotaron %d", rejected)
	}
}

func TestMCPServerMetrics(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.82"
	serverID := f.enrolledServer(t, "vps-metricas")
	now := time.Now().UTC().Truncate(10 * time.Second)
	var samples []proto.Sample
	for i := range 30 {
		at := now.Add(-time.Duration(30-i) * 10 * time.Second)
		samples = append(samples, sample(at, float64(10+i), 2, 45, uint64(i)*1000))
	}
	f.ingest(t, serverID, samples...)
	value, _ := f.mcpToken(t, "metricas", []string{"read:production"})

	session, err := f.mcpSession(t, value, ip)
	if err != nil {
		t.Fatalf("conectar: %v", err)
	}
	res, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: "get_server_metrics", Arguments: map[string]any{"server": "vps-metricas"}})
	if err != nil || res.IsError {
		t.Fatalf("get_server_metrics = %+v, %v", res, err)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out struct {
		Resolution string `json:"resolution"`
		Samples    int    `json:"samples"`
		CPU        struct {
			Last, Avg, Max float64
		} `json:"cpu_percent"`
		Memory struct{ Last float64 } `json:"memory_percent"`
		Swap   *struct{ Max float64 } `json:"swap_percent"`
		Disks  []struct {
			Mountpoint  string  `json:"mountpoint"`
			LastPercent float64 `json:"last_used_percent"`
		} `json:"disks"`
		Series struct {
			Timestamps []string `json:"timestamps"`
		} `json:"series"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Resolution != "raw" || out.Samples != 30 || out.CPU.Last != 39 || out.CPU.Max != 39 || out.Memory.Last != 25 {
		t.Errorf("métricas = %s", raw)
	}
	if out.Swap == nil || out.Swap.Max != 0 {
		t.Errorf("con swap declarado va el campo: %s", raw)
	}
	if len(out.Disks) != 2 || out.Disks[0].Mountpoint != "/" || out.Disks[0].LastPercent != 45 {
		t.Errorf("discos = %+v", out.Disks)
	}
	if n := len(out.Series.Timestamps); n == 0 || n > 60 {
		t.Errorf("la serie tiene %d puntos", n)
	}
}

func TestMCPProjects(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.83"
	projectID := f.declaredProject(t, "vps-proyectos", "tienda", "production")
	f.declaredProject(t, "vps-proyectos-2", "demo", "staging")
	value, _ := f.mcpToken(t, "proyectos", []string{"read:production"})

	session, err := f.mcpSession(t, value, ip)
	if err != nil {
		t.Fatalf("conectar: %v", err)
	}
	structured := func(name string, args map[string]any) map[string]any {
		t.Helper()
		res, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil || res.IsError {
			t.Fatalf("%s = %+v, %v", name, res, err)
		}
		raw, _ := json.Marshal(res.StructuredContent)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	list := structured("list_projects", map[string]any{})
	projects, _ := list["projects"].([]any)
	if len(projects) != 1 {
		t.Fatalf("proyectos = %+v", list)
	}
	if first, _ := projects[0].(map[string]any); first["ref"] != "vps-proyectos/tienda" {
		t.Fatalf("proyectos = %+v", list)
	}

	status := structured("get_project_status", map[string]any{"project": "tienda"})
	project, _ := status["project"].(map[string]any)
	if project["id"] != projectID.String() || project["status"] != "unknown" || status["runtime_source"] != "unknown" || status["version"] != nil {
		t.Errorf("estado sin nada del agent = %+v", status)
	}

	var actions []string
	for _, e := range f.auditFrom(t, ip) {
		actions = append(actions, e.Action+" "+e.TargetType)
	}
	if strings.Join(actions, ",") != "mcp.list_projects mcp,mcp.get_project_status project" {
		t.Errorf("auditoría = %v", actions)
	}
}

func TestMCPDeploys(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.84"
	f.createUser(t)
	projectID := f.declaredProject(t, "vps-deploys", "tienda", "production")
	project, err := f.queries.GetProject(t.Context(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	user, err := f.queries.GetUserByUsername(t.Context(), "juan")
	if err != nil {
		t.Fatal(err)
	}
	deployID := uuid.Must(uuid.NewV7())
	if _, err := deploys.NewService(f.pool).Create(t.Context(), deploys.Request{
		ID: deployID, ServerID: project.ServerID, ProjectID: projectID, ProjectName: "tienda",
		Environment: "production", Kind: proto.DeployKindDeploy, UserID: user.ID, Username: "juan",
	}); err != nil {
		t.Fatalf("crear el deploy: %v", err)
	}
	value, _ := f.mcpToken(t, "deploys", []string{"read:production"})
	session, err := f.mcpSession(t, value, ip)
	if err != nil {
		t.Fatalf("conectar: %v", err)
	}

	res, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: "list_deploys", Arguments: map[string]any{"project": "vps-deploys/tienda"}})
	if err != nil || res.IsError {
		t.Fatalf("list_deploys = %+v, %v", res, err)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(raw), deployID.String()) || !strings.Contains(string(raw), `"status":"pending"`) {
		t.Errorf("list_deploys = %s", raw)
	}

	res, err = session.CallTool(t.Context(), &sdk.CallToolParams{Name: "get_deploy", Arguments: map[string]any{"deploy_id": deployID.String()}})
	if err != nil || res.IsError {
		t.Fatalf("get_deploy = %+v, %v", res, err)
	}
	raw, _ = json.Marshal(res.StructuredContent)
	if !strings.Contains(string(raw), `"project":"vps-deploys/tienda"`) || !strings.Contains(string(raw), `"untrusted_notice"`) {
		t.Errorf("get_deploy = %s", raw)
	}
}
