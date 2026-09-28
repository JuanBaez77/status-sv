package runtimestate

import (
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/outbox"
	"github.com/JuanBaez77/status-sv/proto"
)

// AckTimeout es cuánto se espera el acuse de una foto antes de reenviarla.
const AckTimeout = outbox.AckTimeout

// Outbox es la bandeja de salida de las fotos de runtime.
type Outbox = outbox.Box[proto.RuntimeState]

// NewOutbox arma una bandeja vacía.
func NewOutbox() *Outbox {
	return outbox.New(func(s proto.RuntimeState) time.Time { return s.TakenAt })
}
