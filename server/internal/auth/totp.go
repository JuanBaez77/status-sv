package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 usa HMAC-SHA1, que es lo que entienden todas las apps; HMAC no depende de la resistencia a colisiones de SHA-1
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Parámetros de TOTP (RFC 6238), los que entienden todas las apps de
// autenticación: HMAC-SHA1, 6 dígitos y pasos de 30 segundos (ADR-013).
const (
	TOTPSecretBytes = 20
	TOTPDigits      = 6
	TOTPPeriod      = 30 * time.Second
	// totpSkew es cuántos pasos se aceptan para adelante y para atrás: cubre un
	// teléfono con el reloj un poco corrido y el código que se tipeó justo al
	// cambiar.
	totpSkew = 1
)

// Códigos de recuperación: 80 bits al azar, escritos como 16 caracteres en
// base32 en grupos de cuatro. Con 80 bits alcanza con SHA-256 para guardarlos,
// como con los tokens.
const (
	RecoveryCodeCount = 10
	recoveryCodeBytes = 10
	recoveryCodeChars = 16
)

var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret genera el secreto de un usuario.
func NewTOTPSecret() ([]byte, error) {
	secret := make([]byte, TOTPSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generar el secreto TOTP: %w", err)
	}
	return secret, nil
}

// EncodeTOTPSecret escribe el secreto en base32, como se carga a mano en una app.
func EncodeTOTPSecret(secret []byte) string {
	return base32NoPad.EncodeToString(secret)
}

// TOTPURI arma el enlace otpauth:// que va en el QR.
func TOTPURI(issuer, account string, secret []byte) string {
	values := url.Values{}
	values.Set("secret", EncodeTOTPSecret(secret))
	values.Set("issuer", issuer)
	values.Set("algorithm", "SHA1")
	values.Set("digits", fmt.Sprint(TOTPDigits))
	values.Set("period", fmt.Sprint(int(TOTPPeriod.Seconds())))
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	return "otpauth://totp/" + label + "?" + values.Encode()
}

// TOTPStep es el número de paso de un instante.
func TOTPStep(t time.Time) int64 {
	return t.Unix() / int64(TOTPPeriod.Seconds())
}

// TOTPCode calcula el código de un paso.
func TOTPCode(secret []byte, step int64) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step)) //nolint:gosec // los pasos de un reloj real son positivos

	mac := hmac.New(sha1.New, secret)
	mac.Write(counter[:])
	sum := mac.Sum(nil)

	// Truncado dinámico de la RFC 4226.
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", TOTPDigits, value%1_000_000)
}

// VerifyTOTP dice si code vale en now, y en qué paso. Solo acepta pasos
// posteriores a lastStep: un código ya usado no sirve dos veces, aunque siga
// dentro de su ventana.
func VerifyTOTP(secret []byte, code string, now time.Time, lastStep int64) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != TOTPDigits {
		return 0, false
	}
	current := TOTPStep(now)
	var (
		found int64
		ok    bool
	)
	// Se recorren todos los pasos de la ventana sin cortar al encontrar uno, así
	// el tiempo no dice cuál coincidió.
	for step := current - totpSkew; step <= current+totpSkew; step++ {
		if subtle.ConstantTimeCompare([]byte(TOTPCode(secret, step)), []byte(code)) == 1 && step > lastStep && !ok {
			found, ok = step, true
		}
	}
	return found, ok
}

// NewRecoveryCodes genera los códigos de recuperación de un usuario.
func NewRecoveryCodes() ([]string, error) {
	codes := make([]string, RecoveryCodeCount)
	raw := make([]byte, recoveryCodeBytes)
	for i := range codes {
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("generar un código de recuperación: %w", err)
		}
		plain := base32NoPad.EncodeToString(raw)
		codes[i] = plain[0:4] + "-" + plain[4:8] + "-" + plain[8:12] + "-" + plain[12:16]
	}
	return codes, nil
}

// NormalizeRecoveryCode deja un código como se tipeó (con o sin guiones, en
// minúsculas, con espacios) en su forma canónica. Devuelve false si no puede ser
// un código.
func NormalizeRecoveryCode(input string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(input) {
		switch {
		case r == '-' || r == ' ':
			continue
		case (r >= 'A' && r <= 'Z') || (r >= '2' && r <= '7'):
			b.WriteRune(r)
		default:
			return "", false
		}
	}
	if b.Len() != recoveryCodeChars {
		return "", false
	}
	return b.String(), true
}

// HashRecoveryCode es lo que se guarda de un código ya normalizado.
func HashRecoveryCode(normalized string) []byte {
	sum := sha256.Sum256([]byte(normalized))
	return sum[:]
}
