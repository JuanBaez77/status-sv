// Package auth genera y verifica los secretos del control plane: los tokens de
// enrolamiento de un solo uso, las credenciales propias de cada agent, las
// sesiones del panel, los tokens del MCP y los hashes de las contraseñas.
//
// Dos reglas que valen para todo el paquete:
//
//   - El valor en claro existe solo en memoria y se entrega una única vez. La
//     base guarda nada más que el hash.
//   - Un Secret no se puede imprimir por accidente: String y LogValue devuelven
//     una marca, y hay que pedir Reveal a propósito para ver el valor.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
)

// secretBytes es la entropía de cada secreto. 32 bytes es lo que recomienda
// NIST para material de clave y es lo que hace innecesario un hash lento: no hay
// diccionario que sirva contra 256 bits al azar.
const secretBytes = 32

// HashLen es el largo del hash que se guarda, en bytes. La migración 00001 lo
// exige con una restricción.
const HashLen = sha256.Size

// Prefijos que hacen evidente qué es cada secreto si aparece en un archivo de
// configuración o en un pedido. No son secretos ellos mismos.
const (
	EnrollmentTokenPrefix = "svenr_"
	AgentCredentialPrefix = "svagt_"
	SessionTokenPrefix    = "svses_"
	MCPTokenPrefix        = "svmcp_"
)

// Secret es un secreto en claro.
type Secret struct {
	value string
}

// NewEnrollmentToken crea el token de un solo uso que se le da a un agent nuevo.
func NewEnrollmentToken() (Secret, error) {
	return newSecret(EnrollmentTokenPrefix)
}

// NewSessionToken crea el token de una sesión del panel, el que viaja en la
// cookie.
func NewSessionToken() (Secret, error) {
	return newSecret(SessionTokenPrefix)
}

// NewAgentCredential crea la credencial propia de un agent ya enrolado.
func NewAgentCredential() (Secret, error) {
	return newSecret(AgentCredentialPrefix)
}

// NewMCPToken crea un token del MCP, el que Claude Code manda en cada pedido
// (ADR-025).
func NewMCPToken() (Secret, error) {
	return newSecret(MCPTokenPrefix)
}

func newSecret(prefix string) (Secret, error) {
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return Secret{}, fmt.Errorf("generar bytes al azar: %w", err)
	}
	return Secret{value: prefix + base64.RawURLEncoding.EncodeToString(raw)}, nil
}

// ParseSecret envuelve un secreto que llegó de afuera, para poder tratarlo con
// el mismo cuidado que uno recién generado.
func ParseSecret(value string) Secret {
	return Secret{value: value}
}

// Reveal devuelve el valor en claro. Se llama en un solo lugar de cada flujo:
// cuando el secreto se le entrega a su dueño.
func (s Secret) Reveal() string {
	return s.value
}

// Hash devuelve el SHA-256 que se guarda en la base.
func (s Secret) Hash() []byte {
	return HashOf(s.value)
}

// IsZero dice si el secreto está vacío.
func (s Secret) IsZero() bool {
	return s.value == ""
}

// HasPrefix dice si el secreto arranca con el prefijo esperado. Sirve para
// rechazar temprano una credencial usada donde va un token, sin tocar la base.
func (s Secret) HasPrefix(prefix string) bool {
	return strings.HasPrefix(s.value, prefix)
}

// String oculta el valor: si un secreto termina en un log por descuido, no se filtra.
func (s Secret) String() string {
	return "[secreto oculto]"
}

// LogValue hace lo mismo para slog.
func (s Secret) LogValue() slog.Value {
	return slog.StringValue("[secreto oculto]")
}

// HashOf devuelve el SHA-256 de un secreto en claro.
//
// Es a propósito un hash rápido y no argon2id: estos secretos son 32 bytes al
// azar, no contraseñas elegidas por una persona, y se verifican en cada
// reconexión de cada agent. argon2id queda para las contraseñas del panel.
func HashOf(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

// EqualHash compara dos hashes en tiempo constante.
func EqualHash(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
