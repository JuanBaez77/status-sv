//go:build integration

package api_test

import (
	"encoding/base32"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/server/internal/auth"
)

type meBody struct {
	Username          string `json:"username"`
	Stage             string `json:"stage"`
	RecoveryCodesLeft *int64 `json:"recovery_codes_left"`
}

type enrollBody struct {
	Secret string `json:"secret"`
	URI    string `json:"otpauth_uri"`
	QR     string `json:"qr_png"`
}

type confirmBody struct {
	Stage         string   `json:"stage"`
	RecoveryCodes []string `json:"recovery_codes"`
}

func decodeJSON[T any](t *testing.T, resp response) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(resp.body, &v); err != nil {
		t.Fatalf("leer %s: %v", resp.body, err)
	}
	return v
}

func (f *fixture) passwordLogin(t *testing.T, ip string) *http.Cookie {
	t.Helper()
	resp := f.do(t, http.MethodPost, "/v1/auth/login",
		`{"username":"juan","password":"`+testPassword+`"}`, ip, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d: %s", resp.StatusCode, resp.body)
	}
	return sessionCookieOf(t, resp)
}

// loginWithTOTP inicia sesión con juan, que todavía no tiene segundo factor, y
// lo da de alta. Devuelve la cookie completa y el secreto de la app.
func (f *fixture) loginWithTOTP(t *testing.T, ip string) (*http.Cookie, []byte) {
	t.Helper()
	half := f.passwordLogin(t, ip)

	enroll := f.do(t, http.MethodPost, "/v1/auth/totp/enroll", "", ip, half, nil)
	if enroll.StatusCode != http.StatusOK {
		t.Fatalf("alta = %d: %s", enroll.StatusCode, enroll.body)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(decodeJSON[enrollBody](t, enroll).Secret)
	if err != nil {
		t.Fatalf("el secreto no es base32: %v", err)
	}

	code := auth.TOTPCode(secret, auth.TOTPStep(time.Now()))
	confirm := f.do(t, http.MethodPost, "/v1/auth/totp/confirm", `{"code":"`+code+`"}`, ip, half, nil)
	if confirm.StatusCode != http.StatusOK {
		t.Fatalf("confirmar el alta = %d: %s", confirm.StatusCode, confirm.body)
	}
	return sessionCookieOf(t, confirm), secret
}

func TestTwoStepLoginWithEnrollment(t *testing.T) {
	f := setup(t)
	f.createUser(t)
	const ip = "198.51.100.40"

	half := f.passwordLogin(t, ip)
	if me := decodeJSON[meBody](t, f.do(t, http.MethodGet, "/v1/auth/me", "", ip, half, nil)); me.Stage != "totp_enroll" || me.RecoveryCodesLeft != nil {
		t.Errorf("me a medias = %+v; se esperaba la etapa de alta, sin códigos", me)
	}
	// Con la sesión a medias no se ve nada del panel.
	for _, path := range []string{"/v1/servers", "/v1/panel/connect"} {
		resp := f.do(t, http.MethodGet, path, "", ip, half, nil)
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(resp.body), "second_factor_required") {
			t.Errorf("GET %s a medias = %d %s; se esperaba 401 second_factor_required", path, resp.StatusCode, resp.body)
		}
	}
	// Ni se puede saltear el alta verificando un código.
	if resp := f.do(t, http.MethodPost, "/v1/auth/totp/verify", `{"code":"123456"}`, ip, half, nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("verificar en el alta = %d, se esperaba 409", resp.StatusCode)
	}

	enroll := f.do(t, http.MethodPost, "/v1/auth/totp/enroll", "", ip, half, nil)
	if enroll.StatusCode != http.StatusOK {
		t.Fatalf("alta = %d: %s", enroll.StatusCode, enroll.body)
	}
	body := decodeJSON[enrollBody](t, enroll)
	if !strings.HasPrefix(body.QR, "data:image/png;base64,") || !strings.HasPrefix(body.URI, "otpauth://totp/") || len(body.Secret) != 32 {
		t.Errorf("alta = secreto %d caracteres, URI %q, QR %.30q", len(body.Secret), body.URI, body.QR)
	}
	if got := enroll.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control del alta = %q", got)
	}
	secret, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(body.Secret)

	if resp := f.do(t, http.MethodPost, "/v1/auth/totp/confirm", `{"code":"000000"}`, ip, half, nil); resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(string(resp.body), "invalid_code") {
		t.Errorf("código equivocado = %d %s", resp.StatusCode, resp.body)
	}
	code := auth.TOTPCode(secret, auth.TOTPStep(time.Now()))
	confirm := f.do(t, http.MethodPost, "/v1/auth/totp/confirm", `{"code":"`+code+`"}`, ip, half, nil)
	if confirm.StatusCode != http.StatusOK {
		t.Fatalf("confirmar = %d: %s", confirm.StatusCode, confirm.body)
	}
	confirmed := decodeJSON[confirmBody](t, confirm)
	if confirmed.Stage != "full" || len(confirmed.RecoveryCodes) != auth.RecoveryCodeCount {
		t.Errorf("confirmación = %+v", confirmed)
	}
	full := sessionCookieOf(t, confirm)
	if full.Value == half.Value {
		t.Error("la sesión completa usa el mismo token que la sesión a medias")
	}

	if resp := f.do(t, http.MethodGet, "/v1/servers", "", ip, half, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("la cookie a medias sigue sirviendo: %d", resp.StatusCode)
	}
	if resp := f.do(t, http.MethodGet, "/v1/servers", "", ip, full, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/servers completo = %d", resp.StatusCode)
	}
	me := decodeJSON[meBody](t, f.do(t, http.MethodGet, "/v1/auth/me", "", ip, full, nil))
	if me.Stage != "full" || me.RecoveryCodesLeft == nil || *me.RecoveryCodesLeft != auth.RecoveryCodeCount {
		t.Errorf("me completo = %+v", me)
	}

	// El próximo login pide el código; con uno de recuperación también entra.
	pending := f.passwordLogin(t, "198.51.100.41")
	if me := decodeJSON[meBody](t, f.do(t, http.MethodGet, "/v1/auth/me", "", ip, pending, nil)); me.Stage != "totp_pending" {
		t.Errorf("me del segundo login = %+v", me)
	}
	if resp := f.do(t, http.MethodPost, "/v1/auth/totp/enroll", "", ip, pending, nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("un alta con TOTP activo = %d, se esperaba 409", resp.StatusCode)
	}
	verify := f.do(t, http.MethodPost, "/v1/auth/totp/verify",
		`{"recovery_code":"`+confirmed.RecoveryCodes[0]+`"}`, ip, pending, nil)
	if verify.StatusCode != http.StatusOK {
		t.Fatalf("verificar con código de recuperación = %d: %s", verify.StatusCode, verify.body)
	}
	if me := decodeJSON[meBody](t, verify); me.RecoveryCodesLeft == nil || *me.RecoveryCodesLeft != auth.RecoveryCodeCount-1 {
		t.Errorf("después de usar un código = %+v", me)
	}
}

func TestTOTPRequestsAreValidated(t *testing.T) {
	f := setup(t)
	f.createUser(t)
	const ip = "198.51.100.42"
	f.loginWithTOTP(t, ip)
	pending := f.passwordLogin(t, ip)

	for name, body := range map[string]string{
		"sin código":      `{}`,
		"los dos códigos": `{"code":"123456","recovery_code":"AAAA-AAAA-AAAA-AAAA"}`,
		"campos de más":   `{"code":"123456","admin":true}`,
		"no es JSON":      `code=123456`,
	} {
		if resp := f.do(t, http.MethodPost, "/v1/auth/totp/verify", body, ip, pending, nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, se esperaba 400", name, resp.StatusCode)
		}
	}
	// Sin sesión, ni se mira el código.
	if resp := f.do(t, http.MethodPost, "/v1/auth/totp/verify", `{"code":"123456"}`, ip, nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("sin sesión = %d, se esperaba 401", resp.StatusCode)
	}
	// Desde otro sitio, tampoco.
	cross := map[string]string{"Sec-Fetch-Site": "cross-site"}
	if resp := f.do(t, http.MethodPost, "/v1/auth/totp/verify", `{"code":"123456"}`, ip, pending, cross); resp.StatusCode != http.StatusForbidden {
		t.Errorf("desde otro sitio = %d, se esperaba 403", resp.StatusCode)
	}
}

// Con cinco códigos equivocados la sesión a medias se cierra y se borra la
// cookie: hay que volver a poner la contraseña.
func TestWrongCodesCloseTheHalfSession(t *testing.T) {
	f := setup(t)
	f.createUser(t)
	f.loginWithTOTP(t, "198.51.100.43")
	const ip = "198.51.100.44"
	pending := f.passwordLogin(t, ip)

	var last response
	for range 5 {
		last = f.do(t, http.MethodPost, "/v1/auth/totp/verify", `{"code":"000000"}`, ip, pending, nil)
	}
	if last.StatusCode != http.StatusUnauthorized || !strings.Contains(string(last.body), `"unauthorized"`) {
		t.Fatalf("quinto intento = %d %s", last.StatusCode, last.body)
	}
	if cleared := sessionCookieOf(t, last); cleared.MaxAge >= 0 {
		t.Error("no se borró la cookie de la sesión cerrada")
	}
	if resp := f.do(t, http.MethodGet, "/v1/auth/me", "", ip, pending, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("me después de los intentos = %d, se esperaba 401", resp.StatusCode)
	}
}
