package auth_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/JuanBaez77/status-sv/server/internal/auth"
)

func TestNewSecretsAreUniqueAndPrefixed(t *testing.T) {
	tests := []struct {
		name   string
		create func() (auth.Secret, error)
		prefix string
	}{
		{name: "token de enrolamiento", create: auth.NewEnrollmentToken, prefix: auth.EnrollmentTokenPrefix},
		{name: "credencial de agent", create: auth.NewAgentCredential, prefix: auth.AgentCredentialPrefix},
		{name: "sesión del panel", create: auth.NewSessionToken, prefix: auth.SessionTokenPrefix},
		{name: "token del MCP", create: auth.NewMCPToken, prefix: auth.MCPTokenPrefix},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen := make(map[string]bool, 100)
			for range 100 {
				secret, err := tt.create()
				if err != nil {
					t.Fatalf("generar el secreto: %v", err)
				}
				value := secret.Reveal()

				if !strings.HasPrefix(value, tt.prefix) {
					t.Fatalf("el secreto no arranca con %q: %q", tt.prefix, value)
				}
				if !secret.HasPrefix(tt.prefix) {
					t.Errorf("HasPrefix no reconoce su propio prefijo")
				}
				// 32 bytes en base64 sin relleno son 43 caracteres.
				if got := len(value) - len(tt.prefix); got != 43 {
					t.Fatalf("el cuerpo del secreto mide %d caracteres, se esperaban 43", got)
				}
				if seen[value] {
					t.Fatalf("se repitió un secreto: la fuente de azar no está funcionando")
				}
				seen[value] = true
			}
		})
	}
}

func TestSecretIsNotPrintedByAccident(t *testing.T) {
	secret, err := auth.NewAgentCredential()
	if err != nil {
		t.Fatalf("generar la credencial: %v", err)
	}
	value := secret.Reveal()

	// Las tres formas habituales de que un secreto termine en un log.
	printed := fmt.Sprintf("%v %s %+v", secret, secret, secret)
	if strings.Contains(printed, value) {
		t.Errorf("el secreto se filtró al formatearlo: %q", printed)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.InfoContext(context.Background(), "prueba", "credencial", secret)
	if strings.Contains(buf.String(), value) {
		t.Errorf("el secreto se filtró en el log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "[secreto oculto]") {
		t.Errorf("el log no muestra la marca de secreto oculto: %s", buf.String())
	}
}

func TestHashIsStableAndTheRightSize(t *testing.T) {
	secret, err := auth.NewEnrollmentToken()
	if err != nil {
		t.Fatalf("generar el token: %v", err)
	}

	hash := secret.Hash()
	if len(hash) != auth.HashLen {
		t.Fatalf("el hash mide %d bytes y la base exige %d", len(hash), auth.HashLen)
	}
	if !bytes.Equal(hash, secret.Hash()) {
		t.Error("el hash del mismo secreto cambió entre llamadas")
	}
	// Así es como el servidor verifica lo que llega por la red.
	if !bytes.Equal(hash, auth.HashOf(secret.Reveal())) {
		t.Error("HashOf no coincide con Secret.Hash")
	}
}

func TestDifferentSecretsHashDifferently(t *testing.T) {
	first, err := auth.NewEnrollmentToken()
	if err != nil {
		t.Fatalf("generar el primer token: %v", err)
	}
	second, err := auth.NewEnrollmentToken()
	if err != nil {
		t.Fatalf("generar el segundo token: %v", err)
	}

	if bytes.Equal(first.Hash(), second.Hash()) {
		t.Error("dos secretos distintos dieron el mismo hash")
	}
}

func TestEqualHash(t *testing.T) {
	secret, err := auth.NewAgentCredential()
	if err != nil {
		t.Fatalf("generar la credencial: %v", err)
	}

	if !auth.EqualHash(secret.Hash(), auth.HashOf(secret.Reveal())) {
		t.Error("EqualHash dijo que dos hashes iguales son distintos")
	}
	if auth.EqualHash(secret.Hash(), auth.HashOf("otra cosa")) {
		t.Error("EqualHash dijo que dos hashes distintos son iguales")
	}
	if auth.EqualHash(secret.Hash(), nil) {
		t.Error("EqualHash aceptó un hash vacío")
	}
}

func TestParseSecretRoundTrip(t *testing.T) {
	original, err := auth.NewAgentCredential()
	if err != nil {
		t.Fatalf("generar la credencial: %v", err)
	}

	// Así llega una credencial desde la red: como texto pelado.
	parsed := auth.ParseSecret(original.Reveal())
	if !auth.EqualHash(parsed.Hash(), original.Hash()) {
		t.Error("el secreto no sobrevivió a ParseSecret")
	}
	if parsed.String() != "[secreto oculto]" {
		t.Error("un secreto parseado no oculta su valor")
	}
}

func TestZeroSecret(t *testing.T) {
	var secret auth.Secret
	if !secret.IsZero() {
		t.Error("el secreto vacío no se reconoce como vacío")
	}
	if secret.HasPrefix(auth.AgentCredentialPrefix) {
		t.Error("el secreto vacío dice tener prefijo")
	}
}
