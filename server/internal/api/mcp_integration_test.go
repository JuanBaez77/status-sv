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
