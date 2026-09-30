//go:build integration

package users

import (
	"bytes"
	"encoding/base32"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// codeAt es el código de la app en el reloj del test.
func codeAt(secret []byte, c *clock) string {
	return auth.TOTPCode(secret, auth.TOTPStep(c.Now()))
}

func decodeSecret(t *testing.T, encoded string) []byte {
	t.Helper()
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(encoded)
	if err != nil {
		t.Fatalf("el secreto no es base32: %v", err)
	}
	return secret
}

// loginFull inicia sesión con juan hasta tener una sesión completa. La primera
// vez da de alta el TOTP y deja el secreto en *secret; las siguientes usan el
// código del paso siguiente, porque uno ya usado no sirve dos veces.
func loginFull(t *testing.T, s *Service, c *clock, secret *[]byte) auth.Secret {
	t.Helper()
	_, half, err := s.Login(t.Context(), "juan", password, "203.0.113.7", "Firefox")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	var token auth.Secret
	switch half.Stage {
	case StageTOTPEnroll:
		enrollment, err := s.BeginEnrollment(t.Context(), half)
		if err != nil {
			t.Fatalf("BeginEnrollment: %v", err)
		}
		*secret = decodeSecret(t, enrollment.Secret)
		token, _, _, err = s.ConfirmEnrollment(t.Context(), half, codeAt(*secret, c), "", "")
		if err != nil {
			t.Fatalf("ConfirmEnrollment: %v", err)
		}
	case StageTOTPPending:
		c.Advance(auth.TOTPPeriod)
		token, _, err = s.VerifyTOTP(t.Context(), half, codeAt(*secret, c), "", "")
		if err != nil {
			t.Fatalf("VerifyTOTP: %v", err)
		}
	default:
		t.Fatalf("etapa inesperada después del login: %q", half.Stage)
	}
	return token
}

func TestTOTPEnrollmentAndLogin(t *testing.T) {
	pool, s, c := setup(t)
	user, err := s.CreateUser(t.Context(), "juan", password)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	halfToken, half, err := s.Login(t.Context(), "juan", password, "", "")
	if err != nil || half.Stage != StageTOTPEnroll {
		t.Fatalf("Login = %+v, %v; se esperaba la etapa de alta", half, err)
	}
	// Con una sesión a medias no se puede verificar un código: hay que dar de
	// alta primero.
	if _, _, err := s.VerifyTOTP(t.Context(), half, "123456", "", ""); !errors.Is(err, ErrWrongStage) {
		t.Errorf("VerifyTOTP en el alta = %v, se esperaba ErrWrongStage", err)
	}
	if _, _, _, err := s.ConfirmEnrollment(t.Context(), half, "123456", "", ""); !errors.Is(err, ErrNoPendingEnrollment) {
		t.Errorf("confirmar sin alta = %v, se esperaba ErrNoPendingEnrollment", err)
	}

	enrollment, err := s.BeginEnrollment(t.Context(), half)
	if err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	secret := decodeSecret(t, enrollment.Secret)
	if !strings.HasPrefix(enrollment.URI, "otpauth://totp/status-sv:juan?") {
		t.Errorf("URI = %q", enrollment.URI)
	}

	// En la base queda cifrado, no el secreto.
	stored, err := store.New(pool).GetUserByID(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if stored.TotpPendingSecret == nil || bytes.Contains(stored.TotpPendingSecret, secret) || stored.TotpSecret != nil {
		t.Errorf("se guardó mal el alta: pendiente %x, activo %x", stored.TotpPendingSecret, stored.TotpSecret)
	}

	if _, _, _, err := s.ConfirmEnrollment(t.Context(), half, "000000", "", ""); !errors.Is(err, ErrInvalidCode) {
		t.Errorf("código equivocado = %v, se esperaba ErrInvalidCode", err)
	}
	fullToken, full, codes, err := s.ConfirmEnrollment(t.Context(), half, codeAt(secret, c), "", "")
	if err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}
	if !full.Full() || !full.ExpiresAt.Equal(c.Now().Add(MaxLifetime)) {
		t.Errorf("sesión = %+v; se esperaba completa y de 7 días", full)
	}
	if len(codes) != auth.RecoveryCodeCount {
		t.Errorf("se entregaron %d códigos de recuperación", len(codes))
	}
	// El token de la sesión a medias no sirve más; el nuevo, sí.
	if _, err := s.Authenticate(t.Context(), halfToken); !errors.Is(err, ErrNoSession) {
		t.Error("el token de la sesión a medias sigue sirviendo")
	}
	if got, err := s.Authenticate(t.Context(), fullToken); err != nil || !got.Full() {
		t.Errorf("Authenticate = %+v, %v", got, err)
	}

	// El próximo login pide el código. El que se usó en el alta no sirve de
	// nuevo, aunque siga en su ventana.
	_, pending, err := s.Login(t.Context(), "juan", password, "", "")
	if err != nil || pending.Stage != StageTOTPPending {
		t.Fatalf("segundo Login = %+v, %v; se esperaba la etapa del código", pending, err)
	}
	if _, err := s.BeginEnrollment(t.Context(), pending); !errors.Is(err, ErrWrongStage) {
		t.Errorf("un alta con TOTP ya activo = %v, se esperaba ErrWrongStage", err)
	}
	if _, _, err := s.VerifyTOTP(t.Context(), pending, codeAt(secret, c), "", ""); !errors.Is(err, ErrInvalidCode) {
		t.Errorf("reusar el código = %v, se esperaba ErrInvalidCode", err)
	}
	c.Advance(auth.TOTPPeriod)
	if _, got, err := s.VerifyTOTP(t.Context(), pending, codeAt(secret, c), "", ""); err != nil || !got.Full() {
		t.Errorf("VerifyTOTP = %+v, %v", got, err)
	}
}

func TestTooManyWrongCodesCloseTheHalfSession(t *testing.T) {
	_, s, c := setup(t)
	if _, err := s.CreateUser(t.Context(), "juan", password); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	var secret []byte
	loginFull(t, s, c, &secret)

	token, half, err := s.Login(t.Context(), "juan", password, "", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	for i := 1; i < MaxCodeAttempts; i++ {
		if _, _, err := s.VerifyTOTP(t.Context(), half, "000000", "", ""); !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("intento %d = %v, se esperaba ErrInvalidCode", i, err)
		}
	}
	if _, _, err := s.VerifyTOTP(t.Context(), half, "000000", "", ""); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("último intento = %v, se esperaba ErrTooManyAttempts", err)
	}
	if _, err := s.Authenticate(t.Context(), token); !errors.Is(err, ErrNoSession) {
		t.Error("la sesión a medias sigue viva después de los intentos")
	}
	// Ni siquiera con el código bueno.
	c.Advance(auth.TOTPPeriod)
	if _, _, err := s.VerifyTOTP(t.Context(), half, codeAt(secret, c), "", ""); err == nil {
		t.Error("una sesión cerrada pasó al segundo factor")
	}
}

func TestHalfSessionExpiresAfterFiveMinutes(t *testing.T) {
	_, s, c := setup(t)
	if _, err := s.CreateUser(t.Context(), "juan", password); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	token, _, err := s.Login(t.Context(), "juan", password, "", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	c.Advance(HalfSessionLifetime)
	if _, err := s.Authenticate(t.Context(), token); !errors.Is(err, ErrNoSession) {
		t.Errorf("la sesión a medias sigue viva a los %v: %v", HalfSessionLifetime, err)
	}
}

func TestRecoveryCodesWorkOnce(t *testing.T) {
	_, s, c := setup(t)
	if _, err := s.CreateUser(t.Context(), "juan", password); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_, half, err := s.Login(t.Context(), "juan", password, "", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	enrollment, err := s.BeginEnrollment(t.Context(), half)
	if err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	_, full, codes, err := s.ConfirmEnrollment(t.Context(), half, codeAt(decodeSecret(t, enrollment.Secret), c), "", "")
	if err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}

	_, pending, err := s.Login(t.Context(), "juan", password, "", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	// Se acepta como se tipee: en minúsculas y sin guiones.
	typed := strings.ToLower(strings.ReplaceAll(codes[3], "-", ""))
	if _, got, err := s.VerifyRecoveryCode(t.Context(), pending, typed, "", ""); err != nil || !got.Full() {
		t.Fatalf("VerifyRecoveryCode = %+v, %v", got, err)
	}
	if left, err := s.RecoveryCodesLeft(t.Context(), full.UserID); err != nil || left != auth.RecoveryCodeCount-1 {
		t.Errorf("quedan %d códigos (%v), se esperaban %d", left, err, auth.RecoveryCodeCount-1)
	}

	_, again, err := s.Login(t.Context(), "juan", password, "", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	for _, code := range []string{codes[3], "AAAA-AAAA-AAAA-AAAA", "no es un código"} {
		if _, _, err := s.VerifyRecoveryCode(t.Context(), again, code, "", ""); !errors.Is(err, ErrInvalidCode) {
			t.Errorf("VerifyRecoveryCode(%q) = %v, se esperaba ErrInvalidCode", code, err)
		}
	}
}

func TestResetTOTP(t *testing.T) {
	_, s, c := setup(t)
	user, err := s.CreateUser(t.Context(), "juan", password)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	var secret []byte
	token := loginFull(t, s, c, &secret)

	closed, err := s.ResetTOTP(t.Context(), "Juan")
	if err != nil || closed != 1 {
		t.Fatalf("ResetTOTP = %d, %v; se esperaba cerrar 1 sesión", closed, err)
	}
	if _, err := s.Authenticate(t.Context(), token); !errors.Is(err, ErrNoSession) {
		t.Error("una sesión sobrevivió al reset")
	}
	if left, _ := s.RecoveryCodesLeft(t.Context(), user.ID); left != 0 {
		t.Errorf("quedaron %d códigos de recuperación", left)
	}
	if _, half, err := s.Login(t.Context(), "juan", password, "", ""); err != nil || half.Stage != StageTOTPEnroll {
		t.Errorf("después del reset el login = %+v, %v; se esperaba el alta", half, err)
	}
	if _, err := s.ResetTOTP(t.Context(), "nadie"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("ResetTOTP(nadie) = %v, se esperaba ErrUserNotFound", err)
	}
}

// Un secreto cifrado para un usuario no sirve copiado a otro: la clave lo ata
// a su dueño.
func TestTOTPSecretIsBoundToItsUser(t *testing.T) {
	pool, s, c := setup(t)
	juan, err := s.CreateUser(t.Context(), "juan", password)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	var secret []byte
	loginFull(t, s, c, &secret)
	if _, err := s.CreateUser(t.Context(), "ana", password); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if _, err := pool.Exec(t.Context(), `
		UPDATE users SET totp_secret = (SELECT totp_secret FROM users WHERE id = $1), totp_enabled_at = now()
		WHERE username = 'ana'`, juan.ID); err != nil {
		t.Fatalf("copiar el secreto: %v", err)
	}
	_, half, err := s.Login(t.Context(), "ana", password, "", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	c.Advance(time.Minute)
	if _, _, err := s.VerifyTOTP(t.Context(), half, codeAt(secret, c), "", ""); err == nil ||
		errors.Is(err, ErrInvalidCode) {
		t.Errorf("VerifyTOTP con un secreto copiado = %v, se esperaba que no se pudiera descifrar", err)
	}
}

// Una acción que pide confirmación (un deploy en producción) acepta un código
// nuevo una sola vez, y no acepta el que ya se usó para entrar.
func TestConfirmAction(t *testing.T) {
	_, s, c := setup(t)
	if _, err := s.CreateUser(t.Context(), "juan", password); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	var secret []byte
	token := loginFull(t, s, c, &secret)
	session, err := s.Authenticate(t.Context(), token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	// El código del login ya está usado.
	if err := s.ConfirmAction(t.Context(), session, codeAt(secret, c)); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("con el código del login = %v, se esperaba ErrInvalidCode", err)
	}
	c.Advance(auth.TOTPPeriod)
	code := codeAt(secret, c)
	if err := s.ConfirmAction(t.Context(), session, code); err != nil {
		t.Fatalf("ConfirmAction: %v", err)
	}
	if err := s.ConfirmAction(t.Context(), session, code); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("el mismo código dos veces = %v", err)
	}
	if err := s.ConfirmAction(t.Context(), session, "000000"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("un código inventado = %v", err)
	}
	// Una sesión a medias no confirma nada.
	half := session
	half.Stage = StageTOTPPending
	c.Advance(auth.TOTPPeriod)
	if err := s.ConfirmAction(t.Context(), half, codeAt(secret, c)); !errors.Is(err, ErrWrongStage) {
		t.Fatalf("sesión a medias = %v", err)
	}
	// Un código equivocado no cierra la sesión.
	if _, err := s.Authenticate(t.Context(), token); err != nil {
		t.Fatalf("la sesión se cerró: %v", err)
	}
}
