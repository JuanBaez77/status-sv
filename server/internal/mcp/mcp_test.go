package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JuanBaez77/status-sv/server/internal/audit"
	"github.com/JuanBaez77/status-sv/server/internal/tokens"
)

// fakeBackend devuelve datos fijos.
type fakeBackend struct {
	servers  []ServerInfo
	projects []ProjectInfo
	err      error
	metrics  MetricsInfo
	asked    []metricsAsk
	details  map[uuid.UUID]ProjectDetail
	deploys  map[string][]DeployInfo
	detailed map[uuid.UUID]DeployDetail
}

func (b *fakeBackend) Servers(context.Context) ([]ServerInfo, error) { return b.servers, b.err }

func (b *fakeBackend) Projects(context.Context) ([]ProjectInfo, error) { return b.projects, b.err }

func (b *fakeBackend) Project(_ context.Context, id uuid.UUID) (ProjectDetail, error) {
	d, ok := b.details[id]
	if !ok {
		return ProjectDetail{}, ErrNotFound
	}
	return d, b.err
}

func (b *fakeBackend) ProjectDeploys(_ context.Context, _ uuid.UUID, project string, limit int) ([]DeployInfo, error) {
	list := b.deploys[project]
	return list[:min(limit, len(list))], b.err
}

func (b *fakeBackend) Deploy(_ context.Context, id uuid.UUID) (DeployDetail, error) {
	d, ok := b.detailed[id]
	if !ok {
		return DeployDetail{}, ErrNotFound
	}
	return d, b.err
}

type metricsAsk struct {
	server uuid.UUID
	span   time.Duration
}

func (b *fakeBackend) ServerMetrics(_ context.Context, id uuid.UUID, span time.Duration) (MetricsInfo, error) {
	b.asked = append(b.asked, metricsAsk{server: id, span: span})
	return b.metrics, b.err
}

// fakeRecorder guarda los renglones de auditoría.
type fakeRecorder struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (r *fakeRecorder) Record(_ context.Context, e audit.Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
	return nil
}

func (r *fakeRecorder) all() []audit.Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]audit.Entry(nil), r.entries...)
}

// fixture es un servidor MCP de verdad sobre HTTP, con tokens de prueba: el
// valor del token es su nombre.
type fixture struct {
	backend  *fakeBackend
	recorder *fakeRecorder
	server   *Server
	url      string
	tokens   map[string]tokens.Token
}

var (
	serverA   = uuid.MustParse("0192f5e0-0000-7000-8000-00000000000a")
	serverB   = uuid.MustParse("0192f5e0-0000-7000-8000-00000000000b")
	projShop  = uuid.MustParse("0192f5e0-0000-7000-8000-0000000000a1")
	projDemo  = uuid.MustParse("0192f5e0-0000-7000-8000-0000000000a2")
	projOther = uuid.MustParse("0192f5e0-0000-7000-8000-0000000000b1")
)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	lastSeen := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	f := &fixture{
		backend: &fakeBackend{
			servers: []ServerInfo{
				{
					ID: serverA, Name: "sv-gcg", Hostname: "vps-1", AgentVersion: "0.7.0", Status: "online", LastSeenAt: &lastSeen,
					Sample: &SampleInfo{
						At: lastSeen, CPUPercent: 12.345, MemUsedBytes: 1 << 30, MemTotalBytes: 4 << 30, Load1: 0.456,
						FullestDisk: &DiskInfo{Mountpoint: "/", UsedPercent: 71.26},
					},
				},
				{ID: serverB, Name: "cristales", Hostname: "vps-2", AgentVersion: "0.7.0", Status: "offline", LastSeenAt: &lastSeen},
			},
			projects: []ProjectInfo{
				{ID: projShop, ServerID: serverA, ServerName: "sv-gcg", Name: "tienda", Environment: tokens.EnvProduction},
				{ID: projDemo, ServerID: serverA, ServerName: "sv-gcg", Name: "demo", Environment: tokens.EnvStaging},
				{ID: projOther, ServerID: serverB, ServerName: "cristales", Name: "cotizador", Environment: tokens.EnvProduction},
			},
		},
		recorder: &fakeRecorder{},
		tokens:   map[string]tokens.Token{},
	}
	f.server = New(Options{Backend: f.backend, Recorder: f.recorder, Log: slog.New(slog.DiscardHandler), Version: "test"})

	verify := func(_ context.Context, value string, _ *http.Request) (*sdkauth.TokenInfo, error) {
		tok, ok := f.tokens[value]
		if !ok {
			return nil, sdkauth.ErrInvalidToken
		}
		return TokenInfo(tok, "203.0.113.7"), nil
	}
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return f.server.SDK() },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	srv := httptest.NewServer(sdkauth.RequireBearerToken(verify, nil)(handler))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// addToken registra un token de prueba y lo devuelve.
func (f *fixture) addToken(name string, scopes []tokens.Scope, projects ...uuid.UUID) tokens.Token {
	tok := tokens.Token{
		ID: uuid.Must(uuid.NewV7()), Name: name, Prefix: "svmcp_" + name[:min(6, len(name))],
		Scopes: scopes, ProjectIDs: projects, ExpiresAt: time.Now().Add(time.Hour),
	}
	f.tokens[name] = tok
	return tok
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(req)
}

// connect abre una sesión de cliente MCP con el token.
func (f *fixture) connect(t *testing.T, token string) *sdk.ClientSession {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &sdk.StreamableClientTransport{
		Endpoint:   f.url,
		HTTPClient: &http.Client{Transport: bearer{token: token, next: http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatalf("conectar: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// call llama una herramienta y devuelve su salida estructurada, o el texto del
// error si la herramienta falló.
func call[Out any](t *testing.T, session *sdk.ClientSession, name string, args any) (Out, string) {
	t.Helper()
	var out Out
	res, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("llamar %s: %v", name, err)
	}
	if res.IsError {
		var text []string
		for _, c := range res.Content {
			if tc, ok := c.(*sdk.TextContent); ok {
				text = append(text, tc.Text)
			}
		}
		return out, strings.Join(text, "\n")
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("leer la salida: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("leer la salida %s: %v", raw, err)
	}
	return out, ""
}

func TestToolsAreListedAsReadOnly(t *testing.T) {
	f := newFixture(t)
	f.addToken("lector", []tokens.Scope{"read:staging"})
	session := f.connect(t, "lector")

	res, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) == 0 {
		t.Fatal("no hay herramientas")
	}
	for _, tool := range res.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s no está marcada como de solo lectura", tool.Name)
		}
		if tool.Description == "" {
			t.Errorf("%s no tiene descripción", tool.Name)
		}
	}
	if got := session.InitializeResult(); got == nil || !strings.Contains(got.Instructions, "no confiable") {
		t.Error("las instrucciones tienen que avisar que el texto de los servidores no es confiable")
	}
}

func TestListServersSeesWhatTheTokenReaches(t *testing.T) {
	f := newFixture(t)
	f.addToken("todo-staging", []tokens.Scope{"read:staging"})
	f.addToken("solo-tienda", []tokens.Scope{"read:production", "read:staging"}, projShop)

	all, errText := call[listServersOutput](t, f.connect(t, "todo-staging"), "list_servers", map[string]any{})
	if errText != "" {
		t.Fatalf("list_servers: %s", errText)
	}
	if len(all.Servers) != 2 {
		t.Fatalf("sin lista de proyectos ve todos los servidores, y vio %d", len(all.Servers))
	}
	gcg := all.Servers[0]
	if gcg.Name != "sv-gcg" || strings.Join(gcg.Projects, ",") != "demo" {
		t.Errorf("con read:staging ve solo los proyectos de staging: %+v", gcg)
	}
	if gcg.Usage == nil || gcg.Usage.CPUPercent != 12.3 || gcg.Usage.MemoryPercent != 25 || gcg.Usage.Load1 != 0.46 ||
		gcg.Usage.FullestDisk == nil || gcg.Usage.FullestDisk.UsedPercent != 71.3 {
		t.Errorf("uso = %+v", gcg.Usage)
	}
	if gcg.LastSeenAt != "2026-10-09T12:00:00Z" {
		t.Errorf("último contacto = %q", gcg.LastSeenAt)
	}
	if cristales := all.Servers[1]; cristales.Usage != nil || len(cristales.Projects) != 0 {
		t.Errorf("un servidor sin muestra ni proyectos visibles = %+v", cristales)
	}

	restricted, errText := call[listServersOutput](t, f.connect(t, "solo-tienda"), "list_servers", map[string]any{})
	if errText != "" {
		t.Fatalf("list_servers: %s", errText)
	}
	if len(restricted.Servers) != 1 || restricted.Servers[0].Name != "sv-gcg" || strings.Join(restricted.Servers[0].Projects, ",") != "tienda" {
		t.Errorf("con lista ve solo el servidor de su proyecto: %+v", restricted.Servers)
	}

	entries := f.recorder.all()
	if len(entries) != 2 {
		t.Fatalf("auditoría = %+v", entries)
	}
	e := entries[0]
	if e.Actor != "todo-staging" || e.ActorType != audit.ActorLLM || e.Action != "mcp.list_servers" ||
		e.Result != audit.ResultOK || e.IP != "203.0.113.7" || e.Detail["token"] != "svmcp_todo-s" {
		t.Errorf("renglón = %+v", e)
	}
}

func TestListServersNeedsRead(t *testing.T) {
	f := newFixture(t)
	f.addToken("solo-logs", []tokens.Scope{"logs:production"})

	_, errText := call[listServersOutput](t, f.connect(t, "solo-logs"), "list_servers", map[string]any{})
	if !strings.Contains(errText, "no tiene permiso de lectura") {
		t.Errorf("error = %q", errText)
	}
	if entries := f.recorder.all(); len(entries) != 1 || entries[0].Result != resultDenied {
		t.Errorf("auditoría = %+v", entries)
	}
}

func TestBackendErrorsDoNotLeak(t *testing.T) {
	f := newFixture(t)
	f.addToken("lector", []tokens.Scope{"read:staging"})
	f.backend.err = errors.New("pq: la contraseña de la base es hunter2")

	_, errText := call[listServersOutput](t, f.connect(t, "lector"), "list_servers", map[string]any{})
	if errText != "error interno del control plane" {
		t.Errorf("error = %q: el detalle de un error interno no le llega al LLM", errText)
	}
	if entries := f.recorder.all(); len(entries) != 1 || entries[0].Result != resultError {
		t.Errorf("auditoría = %+v", entries)
	}
}

func TestRateLimitPerToken(t *testing.T) {
	f := newFixture(t)
	f.addToken("apurado", []tokens.Scope{"read:staging"})
	f.addToken("tranquilo", []tokens.Scope{"read:staging"})
	session := f.connect(t, "apurado")

	var limited int
	for range callsBurst + 5 {
		if _, errText := call[listServersOutput](t, session, "list_servers", map[string]any{}); strings.Contains(errText, "demasiadas llamadas") {
			limited++
		}
	}
	if limited < 4 {
		t.Errorf("de %d llamadas seguidas, %d frenadas; se esperaban al menos 4", callsBurst+5, limited)
	}
	var notes int
	for _, e := range f.recorder.all() {
		if e.Result == resultRateLimited {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("el freno se anota una vez por minuto, y se anotó %d veces", notes)
	}

	// El cupo es por token: otro token no se entera.
	if _, errText := call[listServersOutput](t, f.connect(t, "tranquilo"), "list_servers", map[string]any{}); errText != "" {
		t.Errorf("otro token frenado: %s", errText)
	}
}

func TestLogsLimitIsSeparate(t *testing.T) {
	l := newLimiter()
	id := uuid.Must(uuid.NewV7())
	now := time.Now()
	for i := range logsBurst {
		if !l.allow(id, classLogs, now) {
			t.Fatalf("llamada de logs %d frenada antes del cupo", i)
		}
	}
	if l.allow(id, classLogs, now) {
		t.Error("pasado el cupo de logs, get_logs se frena")
	}
	if !l.allow(id, classRead, now) {
		t.Error("el cupo de logs no frena las demás lecturas")
	}
}
