package auth

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Vectores de la RFC 6238 (apéndice B) para SHA-1. La RFC da 8 dígitos; los 6
// que se usan son los últimos seis.
func TestTOTPCodeMatchesRFC6238(t *testing.T) {
	secret := []byte("12345678901234567890")
	tests := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}
	for _, tt := range tests {
		if got := TOTPCode(secret, TOTPStep(time.Unix(tt.unix, 0))); got != tt.want {
			t.Errorf("TOTPCode(%d) = %s, se esperaba %s", tt.unix, got, tt.want)
		}
	}
}

func TestVerifyTOTP(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1111111111, 0)
	step := TOTPStep(now)

	if got, ok := VerifyTOTP(secret, TOTPCode(secret, step), now, 0); !ok || got != step {
		t.Errorf("el código del paso actual no pasó: %d, %v", got, ok)
	}
	// Un paso para atrás o para adelante se acepta: relojes corridos.
	for _, s := range []int64{step - 1, step + 1} {
		if _, ok := VerifyTOTP(secret, TOTPCode(secret, s), now, 0); !ok {
			t.Errorf("el código del paso %d (actual %d) no pasó", s, step)
		}
	}
	// Dos pasos, no.
	if _, ok := VerifyTOTP(secret, TOTPCode(secret, step-2), now, 0); ok {
		t.Error("se aceptó un código de hace dos pasos")
	}
	// Un código ya usado no sirve otra vez, ni uno de un paso anterior.
	if _, ok := VerifyTOTP(secret, TOTPCode(secret, step), now, step); ok {
		t.Error("se aceptó dos veces el mismo código")
	}
	if _, ok := VerifyTOTP(secret, TOTPCode(secret, step-1), now, step); ok {
		t.Error("se aceptó un código anterior al último usado")
	}
	// Con espacios alrededor, sí; con otro largo o con letras, no.
	if _, ok := VerifyTOTP(secret, " "+TOTPCode(secret, step)+"\n", now, 0); !ok {
		t.Error("un código con espacios alrededor no pasó")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := VerifyTOTP(secret, bad, now, 0); ok {
			t.Errorf("se aceptó %q", bad)
		}
	}
}

func TestTOTPURI(t *testing.T) {
	uri := TOTPURI("status-sv", "juan", []byte("12345678901234567890"))
	for _, want := range []string{
		"otpauth://totp/status-sv:juan?",
		"secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ",
		"issuer=status-sv",
		"digits=6",
		"period=30",
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("%s no contiene %s", uri, want)
		}
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, err := NewRecoveryCodes()
	if err != nil {
		t.Fatalf("NewRecoveryCodes: %v", err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Fatalf("se generaron %d códigos, se esperaban %d", len(codes), RecoveryCodeCount)
	}
	seen := map[string]bool{}
	for _, code := range codes {
		if len(code) != 19 || strings.Count(code, "-") != 3 {
			t.Errorf("formato inesperado: %q", code)
		}
		normalized, ok := NormalizeRecoveryCode(strings.ToLower(strings.ReplaceAll(code, "-", " ")))
		if !ok || normalized != strings.ReplaceAll(code, "-", "") {
			t.Errorf("%q no se normaliza bien: %q, %v", code, normalized, ok)
		}
		if seen[code] {
			t.Errorf("código repetido: %q", code)
		}
		seen[code] = true
	}

	for _, bad := range []string{"", "ABCD-EFGH", "ABCD-EFGH-IJKL-MNO1", "ABCD-EFGH-IJKL-MNOP-QRST"} {
		if _, ok := NormalizeRecoveryCode(bad); ok {
			t.Errorf("se aceptó %q", bad)
		}
	}
}

func TestTOTPKeySealAndOpen(t *testing.T) {
	key, err := NewTOTPKey()
	if err != nil {
		t.Fatalf("NewTOTPKey: %v", err)
	}
	secret := []byte("12345678901234567890")

	sealed, err := key.Seal(secret, []byte("usuario-1"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, secret) {
		t.Fatal("el secreto cifrado contiene el secreto en claro")
	}
	opened, err := key.Open(sealed, []byte("usuario-1"))
	if err != nil || !bytes.Equal(opened, secret) {
		t.Fatalf("Open = %q, %v", opened, err)
	}

	// Copiado a otro usuario, no se descifra.
	if _, err := key.Open(sealed, []byte("usuario-2")); err == nil {
		t.Error("se descifró con otro dueño")
	}
	// Con otra clave, tampoco.
	other, _ := NewTOTPKey()
	if _, err := other.Open(sealed, []byte("usuario-1")); err == nil {
		t.Error("se descifró con otra clave")
	}
	// Cifrar dos veces lo mismo da resultados distintos.
	again, _ := key.Seal(secret, []byte("usuario-1"))
	if bytes.Equal(sealed, again) {
		t.Error("dos cifrados iguales: el nonce no cambia")
	}
	if got := key.String(); strings.Contains(got, "=") || got != "[clave TOTP]" {
		t.Errorf("String muestra algo de la clave: %q", got)
	}
}

func TestLoadTOTPKey(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	good := write("buena", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=\n")
	if key, err := LoadTOTPKey(good); err != nil || key.IsZero() {
		t.Errorf("una clave válida no cargó: %v", err)
	}
	for name, content := range map[string]string{
		"corta":     "MDEyMzQ1Njc4OWFiY2RlZg==",
		"no base64": "esto no es base64 !!",
		"vacía":     "",
	} {
		if _, err := LoadTOTPKey(write(name, content)); err == nil {
			t.Errorf("se aceptó una clave %s", name)
		}
	}
	if _, err := LoadTOTPKey(filepath.Join(dir, "no-existe")); err == nil {
		t.Error("se aceptó un archivo que no existe")
	}
}
