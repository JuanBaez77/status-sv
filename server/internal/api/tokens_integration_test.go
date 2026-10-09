//go:build integration

package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

type tokenItem struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Prefix      string    `json:"prefix"`
	Status      string    `json:"status"`
	Scopes      []string  `json:"scopes"`
	AllProjects bool      `json:"all_projects"`
	Projects    []struct {
		ID         uuid.UUID `json:"id"`
		Name       string    `json:"name"`
		ServerName string    `json:"server_name"`
		Missing    bool      `json:"missing"`
	} `json:"projects"`
	CreatedByName string     `json:"created_by_name"`
	ExpiresAt     time.Time  `json:"expires_at"`
	RevokedAt     *time.Time `json:"revoked_at"`
	RevokedByName string     `json:"revoked_by_name"`
}

type tokenListBody struct {
	Tokens      []tokenItem `json:"tokens"`
	MaxDays     int         `json:"max_days"`
	DefaultDays int         `json:"default_days"`
	Scopes      []string    `json:"scopes"`
}

type createdTokenBody struct {
	Token string    `json:"token"`
	Item  tokenItem `json:"item"`
}

// tokenAudit es lo anotado sobre tokens desde una IP: el login también deja
// renglones, que acá no interesan.
func (f *fixture) tokenAudit(t *testing.T, ip string) []auditEntry {
	t.Helper()
	var out []auditEntry
	for _, e := range f.auditFrom(t, ip) {
		if strings.HasPrefix(e.Action, "token.") {
			out = append(out, e)
		}
	}
	return out
}

// declaredProject crea un servidor con un proyecto y devuelve el ID del
// proyecto.
func (f *fixture) declaredProject(t *testing.T, serverName, name, env string) uuid.UUID {
	t.Helper()
	serverID := uuid.Must(uuid.NewV7())
	if _, err := f.queries.CreateServer(t.Context(), store.CreateServerParams{
		ID: serverID, Name: serverName, Hostname: serverName, Labels: []byte(`{}`), AgentVersion: "0.1.0",
	}); err != nil {
		t.Fatalf("crear el servidor: %v", err)
	}
	projectID := uuid.Must(uuid.NewV7())
	if err := f.queries.UpsertProject(t.Context(), store.UpsertProjectParams{
		ID: projectID, ServerID: serverID, Name: name, Environment: env, Repo: "github.com/a/" + name,
		Branch: "main", Runtime: "docker", Workdir: "/srv/" + name, ComposeFile: "compose.yml", ComposeProject: name,
		Units: []string{}, SeenAt: time.Now(),
	}); err != nil {
		t.Fatalf("crear el proyecto: %v", err)
	}
	return projectID
}

func TestTokensCreateListRevoke(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.70"
	f.createUser(t)
	cookie, secret := f.loginWithTOTP(t, ip)
	projectID := f.declaredProject(t, "vps-tokens", "tienda", "production")

	empty := decodeJSON[tokenListBody](t, f.do(t, http.MethodGet, "/v1/tokens", "", ip, cookie, nil))
	if len(empty.Tokens) != 0 || empty.MaxDays != 90 || empty.DefaultDays != 30 {
		t.Errorf("lista vacía = %+v", empty)
	}
	if strings.Join(empty.Scopes, ",") != "read:staging,read:production,logs:staging,logs:production" {
		t.Errorf("scopes ofrecidos = %v", empty.Scopes)
	}

	body := func(name, code string) string {
		return `{"name":"` + name + `","scopes":["read:production","logs:production"],"project_ids":["` +
			projectID.String() + `"],"expires_in_days":7,"totp_code":"` + code + `"}`
	}
	fresh := auth.TOTPCode(secret, auth.TOTPStep(time.Now())+1)

	// Sin código, o con uno equivocado, no hay token.
	if resp := f.do(t, http.MethodPost, "/v1/tokens", body("claude-code", ""), ip, cookie, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("sin código = %d: %s", resp.StatusCode, resp.body)
	}
	// Un pedido inválido se rechaza antes de mirar el código, que no se gasta.
	if resp := f.do(t, http.MethodPost, "/v1/tokens", body("", fresh), ip, cookie, nil); resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(string(resp.body), "falta el nombre") {
		t.Fatalf("sin nombre = %d: %s", resp.StatusCode, resp.body)
	}
	if resp := f.do(t, http.MethodPost, "/v1/tokens", strings.Replace(body("x", fresh), `"expires_in_days":7`, `"expires_in_days":91`, 1), ip, cookie, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("91 días = %d: %s", resp.StatusCode, resp.body)
	}

	resp := f.do(t, http.MethodPost, "/v1/tokens", body("claude-code", fresh), ip, cookie, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("crear = %d: %s", resp.StatusCode, resp.body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("la respuesta con el token no puede quedar en una caché")
	}
	created := decodeJSON[createdTokenBody](t, resp)
	if !strings.HasPrefix(created.Token, auth.MCPTokenPrefix) || !strings.HasPrefix(created.Token, created.Item.Prefix) {
		t.Fatalf("token = %q con prefijo %q", created.Token, created.Item.Prefix)
	}
	item := created.Item
	if item.Status != "active" || item.AllProjects || len(item.Projects) != 1 ||
		item.Projects[0].Name != "tienda" || item.Projects[0].ServerName != "vps-tokens" || item.CreatedByName != "juan" {
		t.Errorf("token creado = %+v", item)
	}
	if want := time.Now().Add(7 * 24 * time.Hour); item.ExpiresAt.Before(want.Add(-time.Minute)) || item.ExpiresAt.After(want.Add(time.Minute)) {
		t.Errorf("vence %v, se esperaban 7 días", item.ExpiresAt)
	}

	// El mismo código no sirve dos veces.
	if resp := f.do(t, http.MethodPost, "/v1/tokens", body("otro", fresh), ip, cookie, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("código repetido = %d: %s", resp.StatusCode, resp.body)
	}

	listResp := f.do(t, http.MethodGet, "/v1/tokens", "", ip, cookie, nil)
	if strings.Contains(string(listResp.body), created.Token) {
		t.Fatal("la lista no puede traer el valor del token")
	}
	list := decodeJSON[tokenListBody](t, listResp)
	if len(list.Tokens) != 1 || list.Tokens[0].ID != item.ID {
		t.Fatalf("lista = %+v", list.Tokens)
	}

	revoke := f.do(t, http.MethodDelete, "/v1/tokens/"+item.ID.String(), "", ip, cookie, nil)
	if revoke.StatusCode != http.StatusOK {
		t.Fatalf("revocar = %d: %s", revoke.StatusCode, revoke.body)
	}
	if got := decodeJSON[tokenItem](t, revoke); got.Status != "revoked" || got.RevokedByName != "juan" {
		t.Errorf("revocado = %+v", got)
	}
	if resp := f.do(t, http.MethodDelete, "/v1/tokens/"+item.ID.String(), "", ip, cookie, nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("revocar dos veces = %d", resp.StatusCode)
	}
	if resp := f.do(t, http.MethodDelete, "/v1/tokens/"+uuid.Must(uuid.NewV7()).String(), "", ip, cookie, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("revocar uno que no existe = %d", resp.StatusCode)
	}

	entries := f.tokenAudit(t, ip)
	wantAudit(t, entries,
		"token.create rejected juan",
		"token.create ok juan",
		"token.create rejected juan",
		"token.revoke ok juan",
	)
	ok := entries[1]
	if ok.TargetType != "api_token" || ok.TargetID != item.ID.String() || ok.Detail["prefijo"] != item.Prefix {
		t.Errorf("renglón de la creación = %+v", ok)
	}
	for _, e := range entries {
		for _, v := range e.Detail {
			if s, isString := v.(string); isString && strings.Contains(created.Token, s) && len(s) > len(item.Prefix) {
				t.Errorf("la auditoría guardó más del token de lo que debía: %v", e.Detail)
			}
		}
	}
}

func TestTokensShowMissingProjects(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.71"
	f.createUser(t)
	cookie, secret := f.loginWithTOTP(t, ip)
	projectID := f.declaredProject(t, "vps-faltante", "efimero", "staging")

	code := auth.TOTPCode(secret, auth.TOTPStep(time.Now())+1)
	resp := f.do(t, http.MethodPost, "/v1/tokens", `{"name":"x","scopes":["read:staging"],"project_ids":["`+
		projectID.String()+`"],"expires_in_days":1,"totp_code":"`+code+`"}`, ip, cookie, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("crear = %d: %s", resp.StatusCode, resp.body)
	}
	if _, err := f.pool.Exec(t.Context(), "DELETE FROM projects WHERE id = $1", projectID); err != nil {
		t.Fatal(err)
	}

	list := decodeJSON[tokenListBody](t, f.do(t, http.MethodGet, "/v1/tokens", "", ip, cookie, nil))
	if len(list.Tokens) != 1 || list.Tokens[0].AllProjects || len(list.Tokens[0].Projects) != 1 || !list.Tokens[0].Projects[0].Missing {
		t.Errorf("un proyecto que ya no está tiene que figurar como faltante, sin pasar a todos: %+v", list.Tokens)
	}
}

func TestTokensNeedASession(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.72"
	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/v1/tokens"},
		{http.MethodPost, "/v1/tokens"},
		{http.MethodDelete, "/v1/tokens/" + uuid.Must(uuid.NewV7()).String()},
	} {
		if resp := f.do(t, req.method, req.path, `{}`, ip, nil, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s sin sesión = %d", req.method, req.path, resp.StatusCode)
		}
	}
}
