package proto

import (
	"errors"
	"fmt"
	"time"
)

// Heartbeat es el "sigo vivo" del agent, cada 15 segundos por defecto. Lleva
// además el consumo del propio agent, que es una de las metas de rendimiento del
// proyecto y así se puede vigilar desde el panel sin instrumentación aparte.
type Heartbeat struct {
	AgentVersion    string  `json:"agent_version"`
	UptimeSeconds   int64   `json:"uptime_seconds"`
	SelfMemoryBytes uint64  `json:"self_memory_bytes"`
	SelfCPUPercent  float64 `json:"self_cpu_percent"`
}

// Type implementa Message.
func (Heartbeat) Type() MessageType { return TypeHeartbeat }

// Validate implementa Message.
func (h Heartbeat) Validate() error {
	switch {
	case h.AgentVersion == "":
		return errors.New("falta la versión del agent")
	case len(h.AgentVersion) > maxVersionLen:
		return fmt.Errorf("la versión del agent supera %d caracteres", maxVersionLen)
	case h.UptimeSeconds < 0:
		return errors.New("el tiempo encendido no puede ser negativo")
	case !isPercent(h.SelfCPUPercent):
		return fmt.Errorf("el CPU del agent está fuera de rango: %v", h.SelfCPUPercent)
	}
	return nil
}

// HeartbeatAck confirma el latido. Trae la hora del servidor, que le sirve al
// agent para detectar relojes corridos, y una configuración nueva solo cuando
// cambió: en el caso normal el acuse es mínimo.
type HeartbeatAck struct {
	ServerTime time.Time    `json:"server_time"`
	Config     *AgentConfig `json:"config,omitempty"`
}

// Type implementa Message.
func (HeartbeatAck) Type() MessageType { return TypeHeartbeatAck }

// Validate implementa Message.
func (h HeartbeatAck) Validate() error {
	if h.ServerTime.IsZero() {
		return errors.New("falta la hora del servidor")
	}
	if h.Config != nil {
		return h.Config.Validate()
	}
	return nil
}

// isPercent acepta porcentajes de 0 a 100. El CPU de una máquina con varios
// núcleos se normaliza al total antes de mandarlo.
func isPercent(v float64) bool {
	return v >= 0 && v <= 100
}
