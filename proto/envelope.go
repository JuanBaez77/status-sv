// Package proto define los mensajes que intercambian el agent y el control plane.
// Es la única fuente de verdad del contrato: los dos extremos importan este paquete.
//
// Todo mensaje viaja dentro de un Envelope con versión, para poder actualizar el
// control plane sin romper a los agents viejos (ver docs/adr/ADR-003).
//
//	{"v":1,"type":"heartbeat","id":"<uuid v7>","ts":"<RFC3339>","data":{}}
package proto

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Version es la versión del protocolo que habla este código. Sube solo cuando se
// quita un campo o cambia su significado; agregar un campo opcional no la mueve.
const Version = 1

// MessageType identifica el contenido de un envelope.
type MessageType string

// Mensajes del agent hacia el control plane.
const (
	TypeEnroll        MessageType = "enroll"
	TypeHeartbeat     MessageType = "heartbeat"
	TypeMetrics       MessageType = "metrics"
	TypeRuntimeState  MessageType = "runtime_state"
	TypeProjectsState MessageType = "projects_state"
	TypeCommandAck    MessageType = "command_ack"
	TypeDeployEvent   MessageType = "deploy_event"
)

// Mensajes del control plane hacia el agent.
const (
	TypeEnrollAck        MessageType = "enroll_ack"
	TypeHeartbeatAck     MessageType = "heartbeat_ack"
	TypeMetricsAck       MessageType = "metrics_ack"
	TypeRuntimeStateAck  MessageType = "runtime_state_ack"
	TypeProjectsStateAck MessageType = "projects_state_ack"
	TypeCommand          MessageType = "command"
	TypeDeployEventAck   MessageType = "deploy_event_ack"
	TypeError            MessageType = "error"
)

// Errores que devuelve el paquete al leer un envelope.
var (
	ErrUnsupportedVersion = errors.New("versión de protocolo no soportada")
	ErrUnknownType        = errors.New("tipo de mensaje desconocido")
	ErrEmptyData          = errors.New("el mensaje no trae datos")
	ErrTypeMismatch       = errors.New("el tipo del envelope no es el del mensaje esperado")
)

// knownTypes es la lista cerrada de tipos que este código entiende. Un tipo que
// no esté acá se rechaza con un error explícito, en vez de ignorarse en silencio.
var knownTypes = map[MessageType]bool{
	TypeEnroll:           true,
	TypeHeartbeat:        true,
	TypeMetrics:          true,
	TypeEnrollAck:        true,
	TypeHeartbeatAck:     true,
	TypeMetricsAck:       true,
	TypeRuntimeState:     true,
	TypeRuntimeStateAck:  true,
	TypeProjectsState:    true,
	TypeProjectsStateAck: true,
	TypeCommand:          true,
	TypeCommandAck:       true,
	TypeDeployEvent:      true,
	TypeDeployEventAck:   true,
	TypeError:            true,
}

// Message es lo que sabe hacer todo mensaje del protocolo: decir de qué tipo es
// y revisar sus propios datos.
type Message interface {
	Type() MessageType
	Validate() error
}

// Envelope es el sobre con el que viaja cualquier mensaje.
type Envelope struct {
	V    int             `json:"v"`
	Type MessageType     `json:"type"`
	ID   uuid.UUID       `json:"id"`
	TS   time.Time       `json:"ts"`
	Data json.RawMessage `json:"data"`
}

// Encode valida el mensaje y lo mete en un envelope nuevo, con su propio ID y la
// marca de tiempo actual en UTC. El ID sirve para el acuse y para que los
// comandos sean idempotentes.
func Encode(m Message) (Envelope, error) {
	if m == nil {
		return Envelope{}, errors.New("no hay mensaje para codificar")
	}
	if err := m.Validate(); err != nil {
		return Envelope{}, fmt.Errorf("mensaje %s inválido: %w", m.Type(), err)
	}

	data, err := json.Marshal(m)
	if err != nil {
		return Envelope{}, fmt.Errorf("serializar el mensaje %s: %w", m.Type(), err)
	}

	id, err := uuid.NewV7()
	if err != nil {
		return Envelope{}, fmt.Errorf("generar el ID del mensaje: %w", err)
	}

	return Envelope{
		V:    Version,
		Type: m.Type(),
		ID:   id,
		TS:   time.Now().UTC(),
		Data: data,
	}, nil
}

// Marshal arma el envelope y lo serializa, que es lo que se escribe en el WebSocket.
func Marshal(m Message) ([]byte, error) {
	envelope, err := Encode(m)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("serializar el envelope: %w", err)
	}
	return raw, nil
}

// ParseEnvelope lee un envelope crudo y revisa lo que no depende del tipo: que la
// versión sea la que hablamos y que el tipo esté en la lista conocida.
func ParseEnvelope(raw []byte) (Envelope, error) {
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Envelope{}, fmt.Errorf("leer el envelope: %w", err)
	}
	if envelope.V != Version {
		return Envelope{}, fmt.Errorf("%w: llegó %d, se habla %d", ErrUnsupportedVersion, envelope.V, Version)
	}
	if !knownTypes[envelope.Type] {
		return Envelope{}, fmt.Errorf("%w: %q", ErrUnknownType, envelope.Type)
	}
	if len(envelope.Data) == 0 {
		return Envelope{}, fmt.Errorf("%w: tipo %q", ErrEmptyData, envelope.Type)
	}
	return envelope, nil
}

// Decode saca el mensaje de adentro del envelope y lo valida. Los campos que el
// envelope traiga de más se ignoran, para que un agent viejo tolere un control
// plane nuevo.
func Decode[T Message](envelope Envelope) (T, error) {
	var msg T

	if envelope.Type != msg.Type() {
		return msg, fmt.Errorf("%w: el envelope es %q y se esperaba %q", ErrTypeMismatch, envelope.Type, msg.Type())
	}
	if len(envelope.Data) == 0 {
		return msg, fmt.Errorf("%w: tipo %q", ErrEmptyData, envelope.Type)
	}
	if err := json.Unmarshal(envelope.Data, &msg); err != nil {
		return msg, fmt.Errorf("leer el mensaje %s: %w", envelope.Type, err)
	}
	if err := msg.Validate(); err != nil {
		return msg, fmt.Errorf("mensaje %s inválido: %w", envelope.Type, err)
	}
	return msg, nil
}
