//go:build integration

package api_test

import (
	"encoding/base32"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/audit"
	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

type auditEntry struct {
	ID         uuid.UUID      `json:"id"`
	At         time.Time      `json:"at"`
	Actor      string         `json:"actor"`
	ActorType  string         `json:"actor_type"`
	Action     string         `json:"action"`
	TargetType string         `json:"target_type"`
	TargetID   string         `json:"target_id"`
	Result     string         `json:"result"`
	IP         string         `json:"ip"`
	Detail     map[string]any `json:"detail"`
}

type auditBody struct {
	Entries    []auditEntry `json:"entries"`
	NextCursor string       `json:"next_cursor"`
}

// auditFrom lee de la base lo que quedó anotado desde una IP, del más viejo al
// más nuevo. audit_log no se vacía entre tests (es solo de inserción), así que
// cada test mira lo suyo por su IP.
func (f *fixture) auditFrom(t *testing.T, ip string) []auditEntry {
	t.Helper()
	rows, err := f.pool.Query(t.Context(),
		"SELECT actor, actor_type, action, target_type, target_id, result, detail FROM audit_log WHERE ip = $1 ORDER BY at, id", ip)
	if err != nil {
		t.Fatalf("leer audit_log: %v", err)
	}
	defer rows.Close()
	var out []auditEntry
	for rows.Next() {
		var (
			e   auditEntry
			raw []byte
		)
		if err := rows.Scan(&e.Actor, &e.ActorType, &e.Action, &e.TargetType, &e.TargetID, &e.Result, &raw); err != nil {
			t.Fatalf("leer un renglón: %v", err)
		}
		if err := json.Unmarshal(raw, &e.Detail); err != nil {
			t.Fatalf("leer el detalle: %v", err)
		}
		out = append(out, e)
	}
	return out
}

// wantAudit compara lo anotado con una lista de "acción resultado actor".
func wantAudit(t *testing.T, got []auditEntry, want ...string) {
	t.Helper()
	var have []string
	for _, e := range got {
		have = append(have, e.Action+" "+e.Result+" "+e.Actor)
	}
	if len(have) != len(want) {
		t.Fatalf("auditoría = %q, se esperaba %q", have, want)
	}
	for i := range want {
		if have[i] != want[i] {
			t.Errorf("renglón %d = %q, se esperaba %q", i, have[i], want[i])
		}
	}
}

func TestAccessIsAudited(t *testing.T) {
	f := setup(t)
	f.createUser(t)
	const ip = "198.51.100.60"

	// Un nombre que no existe y uno que sí, con la contraseña equivocada.
	f.do(t, http.MethodPost, "/v1/auth/login", `{"username":"contraseña-en-el-campo-equivocado","password":"x"}`, ip, nil, nil)
	f.do(t, http.MethodPost, "/v1/auth/login", `{"username":"Juan","password":"no es esta"}`, ip, nil, nil)

	// Login, alta del segundo factor con un código equivocado, y salir.
	half := f.passwordLogin(t, ip)
	if resp := f.do(t, http.MethodPost, "/v1/auth/totp/enroll", "", ip, half, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("alta = %d: %s", resp.StatusCode, resp.body)
	}
	if resp := f.do(t, http.MethodPost, "/v1/auth/totp/confirm", `{"code":"000000"}`, ip, half, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("confirmar con un código equivocado = %d", resp.StatusCode)
	}
	if resp := f.do(t, http.MethodPost, "/v1/auth/logout", "", ip, half, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout = %d", resp.StatusCode)
	}
	// Salir sin sesión no anota nada.
	f.do(t, http.MethodPost, "/v1/auth/logout", "", ip, nil, nil)

	got := f.auditFrom(t, ip)
	wantAudit(t, got,
		"auth.login failed desconocido",
		"auth.login failed juan",
		"auth.login ok juan",
		"auth.totp_enroll failed juan",
		"auth.logout ok juan",
	)
	// Lo que se escribió en el campo del usuario no queda en ningún lado.
	if got[0].TargetID != audit.Unknown || len(got[0].Detail) != 0 {
		t.Errorf("el login con un nombre que no existe guardó algo: %+v", got[0])
	}
	if got[2].Detail["etapa"] != "totp_enroll" {
		t.Errorf("el login no dice en qué etapa quedó: %v", got[2].Detail)
	}
}

func TestSecondFactorIsAudited(t *testing.T) {
	f := setup(t)
	f.createUser(t)
	const ip = "198.51.100.61"

	_, secret := f.loginWithTOTP(t, ip)

	// Otro login: un código equivocado y después el bueno.
	half := f.passwordLogin(t, ip)
	if resp := f.do(t, http.MethodPost, "/v1/auth/totp/verify", `{"code":"000000"}`, ip, half, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("verificar con un código equivocado = %d", resp.StatusCode)
	}
	// El código del alta ya se usó: se entra con el del paso siguiente.
	code := auth.TOTPCode(secret, auth.TOTPStep(time.Now())+1)
	if resp := f.do(t, http.MethodPost, "/v1/auth/totp/verify", `{"code":"`+code+`"}`, ip, half, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("verificar = %d: %s", resp.StatusCode, resp.body)
	}

	wantAudit(t, f.auditFrom(t, ip),
		"auth.login ok juan",
		"auth.totp_enroll ok juan",
		"auth.login ok juan",
		"auth.totp_verify failed juan",
		"auth.totp_verify ok juan",
	)
}

// Entrar con un código de recuperación queda como una acción aparte.
func TestRecoveryCodeIsAudited(t *testing.T) {
	f := setup(t)
	f.createUser(t)
	const ip = "198.51.100.67"

	half := f.passwordLogin(t, ip)
	enroll := f.do(t, http.MethodPost, "/v1/auth/totp/enroll", "", ip, half, nil)
	if enroll.StatusCode != http.StatusOK {
		t.Fatalf("alta = %d: %s", enroll.StatusCode, enroll.body)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(decodeJSON[enrollBody](t, enroll).Secret)
	if err != nil {
		t.Fatalf("el secreto no es base32: %v", err)
	}
	confirm := f.do(t, http.MethodPost, "/v1/auth/totp/confirm",
		`{"code":"`+auth.TOTPCode(secret, auth.TOTPStep(time.Now()))+`"}`, ip, half, nil)
	if confirm.StatusCode != http.StatusOK {
		t.Fatalf("confirmar = %d: %s", confirm.StatusCode, confirm.body)
	}
	codes := decodeJSON[confirmBody](t, confirm).RecoveryCodes

	pending := f.passwordLogin(t, ip)
	if resp := f.do(t, http.MethodPost, "/v1/auth/totp/verify", `{"recovery_code":"no-es-un-codigo"}`, ip, pending, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("verificar con un código de recuperación inventado = %d", resp.StatusCode)
	}
	if resp := f.do(t, http.MethodPost, "/v1/auth/totp/verify", `{"recovery_code":"`+codes[0]+`"}`, ip, pending, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("verificar con un código de recuperación = %d: %s", resp.StatusCode, resp.body)
	}

	got := f.auditFrom(t, ip)
	wantAudit(t, got,
		"auth.login ok juan",
		"auth.totp_enroll ok juan",
		"auth.login ok juan",
		"auth.totp_verify failed juan",
		"auth.recovery_code ok juan",
	)
	if got[3].Detail["con_codigo_de_recuperacion"] != true {
		t.Errorf("el intento fallido no dice que fue con un código de recuperación: %v", got[3].Detail)
	}
}

// Quien golpea el login no escribe un renglón por pedido rechazado.
func TestBlockedLoginLeavesOneRow(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.62"

	for range 12 {
		f.do(t, http.MethodPost, "/v1/auth/login", `{"username":"juan","password":"x"}`, ip, nil, nil)
	}
	blocked := 0
	for _, e := range f.auditFrom(t, ip) {
		if e.Action == audit.ActionLogin && e.Result == audit.ResultBlocked {
			blocked++
			if e.Actor != audit.Unknown {
				t.Errorf("el bloqueo guardó un actor: %q", e.Actor)
			}
		}
	}
	if blocked != 1 {
		t.Errorf("renglones de bloqueo = %d, se esperaba 1", blocked)
	}
}

func TestEnrollmentIsAudited(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.63"

	// Un token sirve una sola vez: el segundo intento se rechaza.
	token := f.newToken(t, "srv-auditado")
	status, envelope := f.postEnroll(t, token, "srv-auditado.example.com", ip)
	if status != http.StatusOK {
		t.Fatalf("enrolar = %d", status)
	}
	ack, err := proto.Decode[proto.EnrollAck](envelope)
	if err != nil {
		t.Fatalf("leer el acuse: %v", err)
	}
	if status, _ := f.postEnroll(t, token, "otra-maquina.example.com", ip); status != http.StatusUnauthorized {
		t.Fatalf("enrolar con un token usado = %d", status)
	}

	got := f.auditFrom(t, ip)
	wantAudit(t, got,
		"agent.enroll ok srv-auditado",
		"agent.enroll rejected desconocido",
	)
	if got[0].ActorType != "agent" || got[0].TargetType != "server" || got[0].TargetID != ack.ServerID.String() {
		t.Errorf("el enrolamiento no apunta al servidor: %+v", got[0])
	}
	// Del pedido rechazado no queda ni el nombre de máquina.
	if got[1].TargetID != audit.Unknown || got[1].Detail["motivo"] == nil || len(got[1].Detail) != 1 {
		t.Errorf("el rechazo guardó algo del pedido: %+v", got[1])
	}
}

func TestAuditListNeedsAFullSession(t *testing.T) {
	f := setup(t)
	f.createUser(t)
	const ip = "198.51.100.64"

	if resp := f.do(t, http.MethodGet, "/v1/audit", "", ip, nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("sin sesión = %d, se esperaba 401", resp.StatusCode)
	}
	// Con la contraseña sola, sin el segundo factor, tampoco.
	if resp := f.do(t, http.MethodGet, "/v1/audit", "", ip, f.passwordLogin(t, ip), nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("con la sesión a medias = %d, se esperaba 401", resp.StatusCode)
	}
}

func TestAuditListFiltersAndPages(t *testing.T) {
	f := setup(t)
	f.createUser(t)
	const ip = "198.51.100.65"
	cookie, _ := f.loginWithTOTP(t, ip)

	// Cinco renglones de un objetivo propio de este test, de los cuales dos
	// son de otra acción.
	target := uuid.Must(uuid.NewV7()).String()
	recorder := audit.NewRecorder(store.New(f.pool))
	actions := []string{audit.ActionDeployRequest, audit.ActionDeployFinish, audit.ActionDeployRequest, audit.ActionDeployFinish, audit.ActionDeployRequest}
	for i, action := range actions {
		err := recorder.Record(t.Context(), audit.Entry{
			Actor: "juan", ActorType: audit.ActorUser, Action: action, TargetType: "project", TargetID: target,
			Result: "accepted", IP: "192.0.2.1", Detail: map[string]any{"n": i},
		})
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	list := func(query url.Values) auditBody {
		t.Helper()
		resp := f.do(t, http.MethodGet, "/v1/audit?"+query.Encode(), "", ip, cookie, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/audit?%s = %d: %s", query.Encode(), resp.StatusCode, resp.body)
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q", got)
		}
		return decodeJSON[auditBody](t, resp)
	}

	// De a dos, del más nuevo al más viejo, sin repetir ni saltear.
	query := url.Values{"target_type": {"project"}, "target_id": {target}, "limit": {"2"}}
	var seen []float64
	for range 5 {
		body := list(query)
		for _, e := range body.Entries {
			n, ok := e.Detail["n"].(float64)
			if !ok {
				t.Fatalf("el detalle no trae el número: %v", e.Detail)
			}
			seen = append(seen, n)
		}
		if body.NextCursor == "" {
			break
		}
		query.Set("before", body.NextCursor)
	}
	if len(seen) != 5 {
		t.Fatalf("renglones leídos = %v", seen)
	}
	for i, n := range seen {
		if n != float64(4-i) {
			t.Fatalf("orden = %v, se esperaba 4 3 2 1 0", seen)
		}
	}

	// Por acción.
	finishes := list(url.Values{"target_id": {target}, "action": {audit.ActionDeployFinish}})
	if len(finishes.Entries) != 2 || finishes.NextCursor != "" {
		t.Fatalf("deploy.finish = %d renglones, cursor %q", len(finishes.Entries), finishes.NextCursor)
	}
	first := finishes.Entries[0]
	if first.Actor != "juan" || first.ActorType != "user" || first.Result != "accepted" || first.IP != "192.0.2.1" || first.At.IsZero() {
		t.Errorf("el renglón no trae todo: %+v", first)
	}

	// Un rango que deja afuera lo de recién, y uno que lo incluye.
	hourAgo := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if old := list(url.Values{"target_id": {target}, "to": {hourAgo}}); len(old.Entries) != 0 {
		t.Errorf("antes de hace una hora = %d renglones", len(old.Entries))
	}
	recent := list(url.Values{"target_id": {target}, "from": {hourAgo}, "actor_type": {"user"}, "result": {"accepted"}})
	if len(recent.Entries) != 5 {
		t.Errorf("de la última hora = %d renglones", len(recent.Entries))
	}

	// Leer la auditoría no se audita: desde esta IP solo quedó el login.
	wantAudit(t, f.auditFrom(t, ip), "auth.login ok juan", "auth.totp_enroll ok juan")
}

func TestAuditListRejectsBadRequests(t *testing.T) {
	f := setup(t)
	f.createUser(t)
	const ip = "198.51.100.66"
	cookie, _ := f.loginWithTOTP(t, ip)

	for name, query := range map[string]string{
		"parámetro desconocido": "actor=juan",
		"parámetro repetido":    "action=auth.login&action=auth.logout",
		"tipo de actor":         "actor_type=robot",
		"acción sin formato":    "action=login",
		"fecha":                 "from=ayer",
		"rango al revés":        "from=2026-10-02T00:00:00Z&to=2026-10-01T00:00:00Z",
		"limit cero":            "limit=0",
		"limit enorme":          "limit=1000",
		"limit con letras":      "limit=muchos",
		"cursor":                "before=abc",
	} {
		resp := f.do(t, http.MethodGet, "/v1/audit?"+query, "", ip, cookie, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: GET /v1/audit?%s = %d, se esperaba 400", name, query, resp.StatusCode)
		}
	}
}
