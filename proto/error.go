package proto

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrorCode es la razón por la que el control plane rechazó un mensaje. Es una
// lista cerrada para que el agent pueda decidir qué hacer sin leer el texto.
type ErrorCode string

const (
	// CodeInvalidToken indica que el token de enrolamiento no existe o ya se usó.
	CodeInvalidToken ErrorCode = "invalid_token"
	// CodeExpiredToken indica que el token existía pero venció.
	CodeExpiredToken ErrorCode = "expired_token"
	// CodeUnauthorized indica que la credencial del agent no vale o fue revocada.
	CodeUnauthorized ErrorCode = "unauthorized"
	// CodeUnsupportedVersion indica que el agent habla una versión de protocolo que el
	// control plane no soporta. El agent tiene que actualizarse.
	CodeUnsupportedVersion ErrorCode = "unsupported_version"
	// CodeInvalidMessage indica que el mensaje no pasó la validación.
	CodeInvalidMessage ErrorCode = "invalid_message"
	// CodeRateLimited indica que llegaron demasiados mensajes y conviene esperar antes de reintentar.
	CodeRateLimited ErrorCode = "rate_limited"
	// CodeInternal indica que falló algo del lado del control plane.
	CodeInternal ErrorCode = "internal"
)

const maxErrorMessageLen = 1024

var knownErrorCodes = map[ErrorCode]bool{
	CodeInvalidToken:       true,
	CodeExpiredToken:       true,
	CodeUnauthorized:       true,
	CodeUnsupportedVersion: true,
	CodeInvalidMessage:     true,
	CodeRateLimited:        true,
	CodeInternal:           true,
}

// ErrorMessage es el rechazo que manda el control plane. El texto es para que
// quede en el log de quien opera; nunca lleva secretos ni datos del token, y el
// agent decide qué hacer mirando el código, no el texto.
type ErrorMessage struct {
	Code      ErrorCode  `json:"code"`
	Message   string     `json:"message"`
	InReplyTo *uuid.UUID `json:"in_reply_to,omitempty"`
}

// Type implementa Message.
func (ErrorMessage) Type() MessageType { return TypeError }

// Validate implementa Message.
func (e ErrorMessage) Validate() error {
	switch {
	case e.Code == "":
		return errors.New("falta el código de error")
	case !knownErrorCodes[e.Code]:
		return fmt.Errorf("código de error desconocido: %q", e.Code)
	case e.Message == "":
		return errors.New("falta el texto del error")
	case len(e.Message) > maxErrorMessageLen:
		return fmt.Errorf("el texto del error supera %d caracteres", maxErrorMessageLen)
	}
	return nil
}

// NewError arma un rechazo como respuesta a un mensaje concreto.
func NewError(code ErrorCode, message string, inReplyTo uuid.UUID) ErrorMessage {
	e := ErrorMessage{Code: code, Message: message}
	if inReplyTo != uuid.Nil {
		e.InReplyTo = &inReplyTo
	}
	return e
}

// Retryable dice si tiene sentido que el agent reintente el mismo mensaje más
// tarde. Un token inválido no mejora con el tiempo; un error interno sí.
func (e ErrorMessage) Retryable() bool {
	switch e.Code {
	case CodeRateLimited, CodeInternal:
		return true
	case CodeInvalidToken, CodeExpiredToken, CodeUnauthorized, CodeUnsupportedVersion, CodeInvalidMessage:
		return false
	default:
		return false
	}
}
