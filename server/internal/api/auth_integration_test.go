//go:build integration

package api_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/JuanBaez77/status-sv/server/internal/api"
	"github.com/JuanBaez77/status-sv/server/internal/users"
)

const testPassword = "una contraseña larga"

// createUser da de alta a juan con testPassword.
func (f *fixture) createUser(t *testing.T) {
	t.Helper()
	service := users.NewService(f.pool, f.totpKey, slog.New(slog.DiscardHandler))
	if _, err := service.CreateUser(t.Context(), "juan", testPassword); err != nil {
		t.Fatalf("crear el usuario: %v", err)
	}
}

// response es lo que interesa de una respuesta, ya leída y cerrada.
type response struct {
	StatusCode int
	Header     http.Header
	cookies    []*http.Cookie
	body       []byte
}

// do manda un pedido. Cada test usa su propia IP, para tener su propio cupo en
// el límite de frecuencia.
func (f *fixture) do(t *testing.T, method, path, body, ip string, cookie *http.Cookie, header map[string]string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("armar el pedido: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("leer la respuesta: %v", err)
	}
	return response{StatusCode: resp.StatusCode, Header: resp.Header, cookies: resp.Cookies(), body: raw}
}

func sessionCookieOf(t *testing.T, resp response) *http.Cookie {
	t.Helper()
	for _, c := range resp.cookies {
		if c.Name == api.SessionCookie {
			return c
		}
	}
	t.Fatalf("la respuesta no trae la cookie de sesión")
	return nil
}

func TestLoginMeLogout(t *testing.T) {
	f := setup(t)
	f.createUser(t)
	const ip = "198.51.100.10"

	resp := f.do(t, http.MethodPost, "/v1/auth/login", `{"username":"Juan","password":"`+testPassword+`"}`, ip, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d: %s", resp.StatusCode, resp.body)
	}
	cookie := sessionCookieOf(t, resp)
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" {
		t.Errorf("cookie sin las protecciones: %+v", cookie)
	}
	if !strings.HasPrefix(cookie.Value, "svses_") {
		t.Errorf("cookie = %q, no es un token de sesión", cookie.Value)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	var body struct{ Username string }
	if err := json.Unmarshal(resp.body, &body); err != nil || body.Username != "juan" {
		t.Errorf("cuerpo del login = %+v, %v", body, err)
	}

	me := f.do(t, http.MethodGet, "/v1/auth/me", "", ip, cookie, nil)
	if me.StatusCode != http.StatusOK {
		t.Fatalf("me = %d", me.StatusCode)
	}

	logout := f.do(t, http.MethodPost, "/v1/auth/logout", "", ip, cookie, nil)
	if logout.StatusCode != http.StatusNoContent {
		t.Fatalf("logout = %d", logout.StatusCode)
	}
	if cleared := sessionCookieOf(t, logout); cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Errorf("el logout no borra la cookie: %+v", cleared)
	}

	if again := f.do(t, http.MethodGet, "/v1/auth/me", "", ip, cookie, nil); again.StatusCode != http.StatusUnauthorized {
		t.Errorf("me después del logout = %d, se esperaba 401", again.StatusCode)
	}
}

func TestMeWithoutSessionIsUnauthorized(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.11"

	if resp := f.do(t, http.MethodGet, "/v1/auth/me", "", ip, nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("sin cookie = %d, se esperaba 401", resp.StatusCode)
	}
	forged := &http.Cookie{Name: api.SessionCookie, Value: "svses_inventado"}
	resp := f.do(t, http.MethodGet, "/v1/auth/me", "", ip, forged, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("cookie inventada = %d, se esperaba 401", resp.StatusCode)
	}
	if cleared := sessionCookieOf(t, resp); cleared.MaxAge >= 0 {
		t.Error("una cookie inválida no se borra")
	}
}

func TestLoginFailures(t *testing.T) {
	f := setup(t)
	f.createUser(t)

	tests := map[string]struct {
		body string
		want int
	}{
		"contraseña equivocada": {`{"username":"juan","password":"no es esta"}`, http.StatusUnauthorized},
		"usuario inexistente":   {`{"username":"nadie","password":"` + testPassword + `"}`, http.StatusUnauthorized},
		"no es JSON":            {`username=juan`, http.StatusBadRequest},
		"campos de más":         {`{"username":"juan","password":"x","admin":true}`, http.StatusBadRequest},
		"cuerpo enorme":         {`{"username":"juan","password":"` + strings.Repeat("a", 8<<10) + `"}`, http.StatusBadRequest},
	}
	i := 0
	for name, tt := range tests {
		i++
		t.Run(name, func(t *testing.T) {
			ip := fmt.Sprintf("198.51.100.%d", 20+i)
			resp := f.do(t, http.MethodPost, "/v1/auth/login", tt.body, ip, nil, nil)
			if resp.StatusCode != tt.want {
				t.Errorf("login = %d, se esperaba %d", resp.StatusCode, tt.want)
			}
			for _, c := range resp.cookies {
				if c.Name == api.SessionCookie {
					t.Error("un login fallido entregó una cookie")
				}
			}
		})
	}
}

// Un formulario de otro sitio no puede iniciar ni cerrar la sesión de Juan:
// el navegador manda Sec-Fetch-Site y el control plane lo rechaza.
func TestCrossSiteRequestsAreRejected(t *testing.T) {
	f := setup(t)
	f.createUser(t)
	const ip = "198.51.100.12"

	crossSite := map[string]string{"Sec-Fetch-Site": "cross-site"}
	resp := f.do(t, http.MethodPost, "/v1/auth/login", `{"username":"juan","password":"`+testPassword+`"}`, ip, nil, crossSite)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("login desde otro sitio = %d, se esperaba 403", resp.StatusCode)
	}

	evilOrigin := map[string]string{"Origin": "https://evil.example"}
	if resp := f.do(t, http.MethodPost, "/v1/auth/logout", "", ip, nil, evilOrigin); resp.StatusCode != http.StatusForbidden {
		t.Errorf("logout desde otro origen = %d, se esperaba 403", resp.StatusCode)
	}

	sameOrigin := map[string]string{"Sec-Fetch-Site": "same-origin"}
	if resp := f.do(t, http.MethodPost, "/v1/auth/login", `{"username":"juan","password":"`+testPassword+`"}`, ip, nil, sameOrigin); resp.StatusCode != http.StatusOK {
		t.Errorf("login del mismo origen = %d, se esperaba 200", resp.StatusCode)
	}
}

func TestLoginIsRateLimited(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.13"

	var last int
	for range 6 {
		last = f.do(t, http.MethodPost, "/v1/auth/login", `{"username":"juan","password":"x"}`, ip, nil, nil).StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("el sexto intento = %d, se esperaba 429", last)
	}
	// Otra IP no paga por la primera.
	if other := f.do(t, http.MethodPost, "/v1/auth/login", `{"username":"juan","password":"x"}`, "198.51.100.14", nil, nil); other.StatusCode != http.StatusUnauthorized {
		t.Errorf("otra IP = %d, se esperaba 401", other.StatusCode)
	}
}
