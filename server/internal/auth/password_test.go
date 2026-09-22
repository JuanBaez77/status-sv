package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashAndVerifyPassword(t *testing.T) {
	const password = "una contraseña larga"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("hash = %q, no es argon2id con los parámetros esperados", hash)
	}
	if strings.Contains(hash, password) {
		t.Fatal("el hash contiene la contraseña")
	}

	ok, err := VerifyPassword(password, hash)
	if err != nil || !ok {
		t.Errorf("VerifyPassword(correcta) = %v, %v", ok, err)
	}
	ok, err = VerifyPassword(password+"x", hash)
	if err != nil || ok {
		t.Errorf("VerifyPassword(incorrecta) = %v, %v", ok, err)
	}

	// Dos hashes de la misma contraseña son distintos: cada uno lleva su sal.
	again, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if again == hash {
		t.Error("dos hashes de la misma contraseña son iguales: falta la sal")
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	valid, err := HashPassword("una contraseña larga")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	parts := strings.Split(valid, "$")

	tests := map[string]string{
		"vacío":             "",
		"bcrypt":            "$2a$10$abcdefghijklmnopqrstuv",
		"argon2i":           strings.Replace(valid, "$argon2id$", "$argon2i$", 1),
		"otra versión":      strings.Replace(valid, "v=19", "v=16", 1),
		"memoria absurda":   strings.Replace(valid, "m=19456", "m=99999999", 1),
		"sin pasadas":       strings.Replace(valid, "t=2", "t=0", 1),
		"sal no base64":     strings.Join([]string{"", parts[1], parts[2], parts[3], "***", parts[5]}, "$"),
		"hash corto":        strings.Join([]string{"", parts[1], parts[2], parts[3], parts[4], "AAAA"}, "$"),
		"partes de más":     valid + "$extra",
		"parámetros rotos":  strings.Replace(valid, "m=19456,t=2,p=1", "m=x", 1),
		"hilos fuera rango": strings.Replace(valid, "p=1", "p=200", 1),
	}
	for name, hash := range tests {
		t.Run(name, func(t *testing.T) {
			ok, err := VerifyPassword("una contraseña larga", hash)
			if ok || !errors.Is(err, ErrMalformedHash) {
				t.Errorf("VerifyPassword = %v, %v; se esperaba ErrMalformedHash", ok, err)
			}
		})
	}
}

func TestNeedsRehash(t *testing.T) {
	current, err := HashPassword("una contraseña larga")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if NeedsRehash(current) {
		t.Error("un hash con los parámetros actuales pide rehash")
	}
	if !NeedsRehash(strings.Replace(current, "t=2", "t=1", 1)) {
		t.Error("un hash con parámetros viejos no pide rehash")
	}
	if !NeedsRehash("ilegible") {
		t.Error("un hash ilegible no pide rehash")
	}
}

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		ok       bool
	}{
		{"corta", "once letras", false},
		{"justa", "doce letras!", true},
		// Se cuentan caracteres, no bytes: "ñ" son dos bytes.
		{"con acentos", "ñññññññññññ", false},
		{"larga", strings.Repeat("a", MaxPasswordLen), true},
		{"demasiado larga", strings.Repeat("a", MaxPasswordLen+1), false},
		{"no UTF-8", "doce letras\xff", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePassword(tt.password)
			if (err == nil) != tt.ok {
				t.Errorf("ValidatePassword(%q) = %v, se esperaba ok=%v", tt.password, err, tt.ok)
			}
			if err != nil && !errors.Is(err, ErrWeakPassword) {
				t.Errorf("el error no envuelve ErrWeakPassword: %v", err)
			}
		})
	}
}

func TestSessionTokenHasItsPrefix(t *testing.T) {
	token, err := NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}
	if !token.HasPrefix(SessionTokenPrefix) || token.HasPrefix(AgentCredentialPrefix) {
		t.Errorf("token = %q, sin el prefijo de sesión", token.Reveal())
	}
}
