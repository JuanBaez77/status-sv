package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// totpKeyBytes es el largo de la clave: AES-256.
const totpKeyBytes = 32

// maxKeyFileBytes acota lo que se lee del archivo de la clave: la clave en
// base64 son 44 caracteres.
const maxKeyFileBytes = 1 << 10

// ErrTOTPKey es una clave que falta o que no sirve.
var ErrTOTPKey = errors.New("clave de TOTP inválida")

// TOTPKey cifra los secretos TOTP que se guardan en la base (ADR-013). Vive
// fuera de la base, en un archivo: un volcado de la base, sin el archivo, no
// sirve para generar códigos.
//
// Igual que Secret, no se puede imprimir por accidente.
type TOTPKey struct {
	aead cipher.AEAD
}

// ParseTOTPKey lee una clave escrita en base64 (la salida de
// `openssl rand -base64 32`).
func ParseTOTPKey(encoded string) (TOTPKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return TOTPKey{}, fmt.Errorf("%w: no es base64", ErrTOTPKey)
	}
	if len(raw) != totpKeyBytes {
		return TOTPKey{}, fmt.Errorf("%w: tiene %d bytes, tienen que ser %d", ErrTOTPKey, len(raw), totpKeyBytes)
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return TOTPKey{}, fmt.Errorf("%w: %w", ErrTOTPKey, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return TOTPKey{}, fmt.Errorf("%w: %w", ErrTOTPKey, err)
	}
	return TOTPKey{aead: aead}, nil
}

// LoadTOTPKey lee la clave de un archivo.
func LoadTOTPKey(path string) (TOTPKey, error) {
	f, err := os.Open(path) //nolint:gosec // la ruta la pone quien configura el control plane
	if err != nil {
		return TOTPKey{}, fmt.Errorf("%w: abrir %s: %w", ErrTOTPKey, path, err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, maxKeyFileBytes)
	n, err := f.Read(buf)
	if err != nil {
		return TOTPKey{}, fmt.Errorf("%w: leer %s: %w", ErrTOTPKey, path, err)
	}
	return ParseTOTPKey(string(buf[:n]))
}

// NewTOTPKey genera una clave al azar. Es para los tests; la de producción se
// genera con openssl y queda en un archivo.
func NewTOTPKey() (TOTPKey, error) {
	raw := make([]byte, totpKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return TOTPKey{}, fmt.Errorf("generar la clave: %w", err)
	}
	return ParseTOTPKey(base64.StdEncoding.EncodeToString(raw))
}

// IsZero dice si la clave no se cargó.
func (k TOTPKey) IsZero() bool {
	return k.aead == nil
}

// Seal cifra un secreto. associated ata el resultado a su dueño (el ID del
// usuario): un secreto copiado a otra fila no se descifra.
func (k TOTPKey) Seal(plaintext, associated []byte) ([]byte, error) {
	if k.IsZero() {
		return nil, fmt.Errorf("%w: no está cargada", ErrTOTPKey)
	}
	nonce := make([]byte, k.aead.NonceSize(), k.aead.NonceSize()+len(plaintext)+k.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generar el nonce: %w", err)
	}
	return k.aead.Seal(nonce, nonce, plaintext, associated), nil
}

// Open descifra lo que cifró Seal con la misma clave y el mismo dueño.
func (k TOTPKey) Open(sealed, associated []byte) ([]byte, error) {
	if k.IsZero() {
		return nil, fmt.Errorf("%w: no está cargada", ErrTOTPKey)
	}
	size := k.aead.NonceSize()
	if len(sealed) < size+k.aead.Overhead() {
		return nil, errors.New("secreto cifrado demasiado corto")
	}
	plaintext, err := k.aead.Open(nil, sealed[:size], sealed[size:], associated)
	if err != nil {
		return nil, fmt.Errorf("descifrar el secreto: %w", err)
	}
	return plaintext, nil
}

// String y LogValue no muestran nada de la clave.
func (k TOTPKey) String() string { return "[clave TOTP]" }

// LogValue implementa slog.LogValuer.
func (k TOTPKey) LogValue() slog.Value { return slog.StringValue("[clave TOTP]") }
