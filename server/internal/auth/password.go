package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Parámetros de argon2id: la primera configuración que recomienda OWASP
// (19 MiB, 2 pasadas, 1 hilo). Con más memoria el control plane se acercaría a
// su tope de 100 MB con dos inicios de sesión a la vez; el login además tiene
// límite de frecuencia, y los hashes llevan sus parámetros, así que subirlos
// más adelante no rompe los que ya existen (NeedsRehash).
const (
	argonMemoryKiB = 19 * 1024
	argonTime      = 2
	argonThreads   = 1
	argonSaltLen   = 16
	argonKeyLen    = 32
)

// Límites de una contraseña. Sin reglas de composición (NIST SP 800-63B): lo que
// cuenta es el largo. El máximo evita que alguien mande un megabyte para que el
// servidor lo procese.
const (
	MinPasswordLen = 12
	MaxPasswordLen = 128
)

// ErrWeakPassword es una contraseña fuera de los límites.
var ErrWeakPassword = errors.New("contraseña inválida")

// ErrMalformedHash es un hash guardado que no se puede leer.
var ErrMalformedHash = errors.New("hash de contraseña ilegible")

// ValidatePassword revisa los límites de una contraseña nueva.
func ValidatePassword(password string) error {
	n := utf8.RuneCountInString(password)
	switch {
	case !utf8.ValidString(password):
		return fmt.Errorf("%w: no es texto UTF-8 válido", ErrWeakPassword)
	case n < MinPasswordLen:
		return fmt.Errorf("%w: tiene que tener al menos %d caracteres", ErrWeakPassword, MinPasswordLen)
	case n > MaxPasswordLen:
		return fmt.Errorf("%w: puede tener como mucho %d caracteres", ErrWeakPassword, MaxPasswordLen)
	}
	return nil
}

// HashPassword devuelve el hash argon2id de una contraseña, en formato PHC.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generar la sal: %w", err)
	}
	p := argonParams{memory: argonMemoryKiB, time: argonTime, threads: argonThreads}
	key := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, argonKeyLen)
	return p.encode(salt, key), nil
}

// VerifyPassword dice si la contraseña corresponde al hash. Compara en tiempo
// constante.
func VerifyPassword(password, encoded string) (bool, error) {
	p, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	// Un password de más de MaxPasswordLen nunca pudo guardarse: se rechaza sin
	// gastar un hash en él.
	if utf8.RuneCountInString(password) > MaxPasswordLen {
		return false, nil
	}
	got := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, uint32(len(key))) //nolint:gosec // el largo sale de un hash de 32 bytes
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

// NeedsRehash dice si el hash se hizo con parámetros distintos de los actuales.
// Al iniciar sesión, con la contraseña en la mano, se vuelve a hashear.
func NeedsRehash(encoded string) bool {
	p, _, key, err := decodeHash(encoded)
	if err != nil {
		return true
	}
	return p.memory != argonMemoryKiB || p.time != argonTime || p.threads != argonThreads || len(key) != argonKeyLen
}

// dummyHash es un hash válido de una contraseña al azar. Cuando el usuario no
// existe, el login verifica contra este: tarda lo mismo que con un usuario real
// y así el tiempo de respuesta no dice qué nombres existen. Se arma la primera
// vez que hace falta, no al arrancar.
var dummyHash = sync.OnceValue(func() string {
	h, err := HashPassword(rand.Text())
	if err != nil {
		panic(fmt.Sprintf("armar el hash de relleno: %v", err))
	}
	return h
})

// BurnPasswordCheck hace el mismo trabajo que VerifyPassword, sin resultado.
func BurnPasswordCheck(password string) {
	_, _ = VerifyPassword(password, dummyHash())
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func (p argonParams) encode(salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memory, p.time, p.threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

// Topes al leer un hash: uno guardado con parámetros absurdos no puede hacer
// que un login use gigas de memoria.
const (
	maxArgonMemoryKiB = 256 * 1024
	maxArgonTime      = 10
	maxArgonThreads   = 8
)

func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", sal, hash
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, ErrMalformedHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return argonParams{}, nil, nil, fmt.Errorf("%w: versión %q", ErrMalformedHash, parts[2])
	}

	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return argonParams{}, nil, nil, fmt.Errorf("%w: parámetros %q", ErrMalformedHash, parts[3])
	}
	if p.memory == 0 || p.memory > maxArgonMemoryKiB || p.time == 0 || p.time > maxArgonTime ||
		p.threads == 0 || p.threads > maxArgonThreads {
		return argonParams{}, nil, nil, fmt.Errorf("%w: parámetros fuera de rango", ErrMalformedHash)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return argonParams{}, nil, nil, fmt.Errorf("%w: sal", ErrMalformedHash)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > 64 {
		return argonParams{}, nil, nil, fmt.Errorf("%w: hash", ErrMalformedHash)
	}
	return p, salt, key, nil
}
