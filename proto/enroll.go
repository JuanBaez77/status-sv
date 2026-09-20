package proto

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Límites de los campos de texto. Están para acotar lo que se acepta de la red,
// no para validar contenido: un mensaje más grande se rechaza sin leerlo entero.
const (
	maxTokenLen      = 512
	minTokenLen      = 20
	maxHostnameLen   = 253
	maxVersionLen    = 64
	maxServerNameLen = 128
	maxPlatformLen   = 32
)

// Enroll es el primer mensaje del agent: presenta el token de un solo uso que se
// generó desde el panel. Viaja siempre sobre TLS y nunca se escribe en los logs.
type Enroll struct {
	Token        string `json:"token"`
	Hostname     string `json:"hostname"`
	AgentVersion string `json:"agent_version"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
}

// Type implementa Message.
func (Enroll) Type() MessageType { return TypeEnroll }

// Validate implementa Message.
func (e Enroll) Validate() error {
	switch {
	case e.Token == "":
		return errors.New("falta el token de enrolamiento")
	case len(e.Token) < minTokenLen:
		return fmt.Errorf("el token es más corto que %d caracteres", minTokenLen)
	case len(e.Token) > maxTokenLen:
		return fmt.Errorf("el token supera %d caracteres", maxTokenLen)
	case e.Hostname == "":
		return errors.New("falta el hostname")
	case len(e.Hostname) > maxHostnameLen:
		return fmt.Errorf("el hostname supera %d caracteres", maxHostnameLen)
	case e.AgentVersion == "":
		return errors.New("falta la versión del agent")
	case len(e.AgentVersion) > maxVersionLen:
		return fmt.Errorf("la versión del agent supera %d caracteres", maxVersionLen)
	case len(e.OS) > maxPlatformLen || len(e.Arch) > maxPlatformLen:
		return fmt.Errorf("el sistema operativo o la arquitectura superan %d caracteres", maxPlatformLen)
	}
	return nil
}

// EnrollAck es la respuesta del control plane: el servidor quedó registrado y
// esta es la credencial propia del agent.
//
// La credencial viaja una sola vez, en este mensaje. El agent la guarda con
// permisos 0600 y el control plane solo conserva su hash: si se pierde, hay que
// enrolar de nuevo.
type EnrollAck struct {
	ServerID   uuid.UUID   `json:"server_id"`
	ServerName string      `json:"server_name"`
	Credential string      `json:"credential"`
	Config     AgentConfig `json:"config"`
}

// Type implementa Message.
func (EnrollAck) Type() MessageType { return TypeEnrollAck }

// Validate implementa Message.
func (e EnrollAck) Validate() error {
	switch {
	case e.ServerID == uuid.Nil:
		return errors.New("falta el ID del servidor")
	case e.ServerName == "":
		return errors.New("falta el nombre del servidor")
	case len(e.ServerName) > maxServerNameLen:
		return fmt.Errorf("el nombre del servidor supera %d caracteres", maxServerNameLen)
	case e.Credential == "":
		return errors.New("falta la credencial")
	case len(e.Credential) > maxTokenLen:
		return fmt.Errorf("la credencial supera %d caracteres", maxTokenLen)
	}
	return e.Config.Validate()
}

// AgentConfig son los intervalos con los que trabaja el agent. Los manda el
// control plane al enrolar y puede cambiarlos después, así que no se compilan
// dentro del agent.
type AgentConfig struct {
	HeartbeatSeconds       int `json:"heartbeat_seconds"`
	MetricsIntervalSeconds int `json:"metrics_interval_seconds"`
	MetricsBatchSeconds    int `json:"metrics_batch_seconds"`
}

// Rangos aceptados para la configuración del agent.
const (
	minHeartbeatSeconds = 5
	maxHeartbeatSeconds = 120
	minMetricsSeconds   = 1
	maxMetricsSeconds   = 300
	maxBatchSeconds     = 600
)

// DefaultAgentConfig son los valores del diseño: latido cada 15 segundos y
// muestras cada 10, enviadas en lotes de 10 segundos.
func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		HeartbeatSeconds:       15,
		MetricsIntervalSeconds: 10,
		MetricsBatchSeconds:    10,
	}
}

// Heartbeat devuelve el intervalo del latido como duración.
func (c AgentConfig) Heartbeat() time.Duration {
	return time.Duration(c.HeartbeatSeconds) * time.Second
}

// Validate revisa que los intervalos estén en rango y sean coherentes entre sí.
func (c AgentConfig) Validate() error {
	switch {
	case c.HeartbeatSeconds < minHeartbeatSeconds || c.HeartbeatSeconds > maxHeartbeatSeconds:
		return fmt.Errorf("el latido debe estar entre %d y %d segundos, llegó %d",
			minHeartbeatSeconds, maxHeartbeatSeconds, c.HeartbeatSeconds)
	case c.MetricsIntervalSeconds < minMetricsSeconds || c.MetricsIntervalSeconds > maxMetricsSeconds:
		return fmt.Errorf("el intervalo de métricas debe estar entre %d y %d segundos, llegó %d",
			minMetricsSeconds, maxMetricsSeconds, c.MetricsIntervalSeconds)
	case c.MetricsBatchSeconds < c.MetricsIntervalSeconds:
		return fmt.Errorf("el lote de métricas (%d s) no puede ser más corto que el intervalo de muestreo (%d s)",
			c.MetricsBatchSeconds, c.MetricsIntervalSeconds)
	case c.MetricsBatchSeconds > maxBatchSeconds:
		return fmt.Errorf("el lote de métricas supera %d segundos", maxBatchSeconds)
	}
	return nil
}
