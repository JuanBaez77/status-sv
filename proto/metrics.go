package proto

import (
	"errors"
	"fmt"
	"time"
)

// Límites del lote de métricas. Con muestras cada 10 segundos, 360 muestras son
// una hora: alcanza de sobra para vaciar el buffer después de una reconexión y
// pone un techo a lo que el control plane tiene que procesar de una vez.
//
// Están exportados para que el colector del agent recorte con los mismos
// números con los que valida el control plane: una muestra que no entra en el
// contrato se descubre en el agent, no como un lote rechazado.
const (
	MaxSamplesPerBatch   = 360
	MaxDisksPerSample    = 64
	MaxNetworksPerSample = 64
	MaxDeviceNameLen     = 128
)

// Metrics es un lote de muestras. El agent manda lotes en vez de muestras
// sueltas para gastar menos red y menos CPU, y acumula en memoria mientras no
// hay conexión.
type Metrics struct {
	Samples []Sample `json:"samples"`
}

// Type implementa Message.
func (Metrics) Type() MessageType { return TypeMetrics }

// Validate implementa Message.
func (m Metrics) Validate() error {
	if len(m.Samples) == 0 {
		return errors.New("el lote de métricas no trae muestras")
	}
	if len(m.Samples) > MaxSamplesPerBatch {
		return fmt.Errorf("el lote trae %d muestras y el máximo es %d", len(m.Samples), MaxSamplesPerBatch)
	}
	for i, sample := range m.Samples {
		if err := sample.Validate(); err != nil {
			return fmt.Errorf("muestra %d: %w", i, err)
		}
	}
	return nil
}

// Sample es el estado del servidor en un instante.
type Sample struct {
	TS       time.Time       `json:"ts"`
	CPU      CPUSample       `json:"cpu"`
	Memory   MemorySample    `json:"memory"`
	Load     LoadSample      `json:"load"`
	Disks    []DiskSample    `json:"disks,omitempty"`
	Networks []NetworkSample `json:"networks,omitempty"`
}

// Validate revisa la muestra entera.
func (s Sample) Validate() error {
	if s.TS.IsZero() {
		return errors.New("falta la marca de tiempo")
	}
	if err := s.CPU.Validate(); err != nil {
		return fmt.Errorf("cpu: %w", err)
	}
	if err := s.Memory.Validate(); err != nil {
		return fmt.Errorf("memoria: %w", err)
	}
	if err := s.Load.Validate(); err != nil {
		return fmt.Errorf("carga: %w", err)
	}
	if len(s.Disks) > MaxDisksPerSample {
		return fmt.Errorf("la muestra trae %d discos y el máximo es %d", len(s.Disks), MaxDisksPerSample)
	}
	for i, disk := range s.Disks {
		if err := disk.Validate(); err != nil {
			return fmt.Errorf("disco %d: %w", i, err)
		}
	}
	if len(s.Networks) > MaxNetworksPerSample {
		return fmt.Errorf("la muestra trae %d interfaces y el máximo es %d", len(s.Networks), MaxNetworksPerSample)
	}
	for i, network := range s.Networks {
		if err := network.Validate(); err != nil {
			return fmt.Errorf("interfaz %d: %w", i, err)
		}
	}
	return nil
}

// CPUSample es el uso de CPU normalizado al total de la máquina.
type CPUSample struct {
	UsedPercent float64 `json:"used_percent"`
	Cores       int     `json:"cores"`
}

// Validate revisa el rango del porcentaje y la cantidad de núcleos.
func (c CPUSample) Validate() error {
	if !isPercent(c.UsedPercent) {
		return fmt.Errorf("uso fuera de rango: %v", c.UsedPercent)
	}
	if c.Cores < 0 {
		return errors.New("la cantidad de núcleos no puede ser negativa")
	}
	return nil
}

// MemorySample es el uso de memoria y de swap, en bytes.
type MemorySample struct {
	TotalBytes     uint64 `json:"total_bytes"`
	UsedBytes      uint64 `json:"used_bytes"`
	SwapTotalBytes uint64 `json:"swap_total_bytes"`
	SwapUsedBytes  uint64 `json:"swap_used_bytes"`
}

// Validate revisa que lo usado no supere lo total.
func (m MemorySample) Validate() error {
	if m.UsedBytes > m.TotalBytes {
		return fmt.Errorf("la memoria usada (%d) supera la total (%d)", m.UsedBytes, m.TotalBytes)
	}
	if m.SwapUsedBytes > m.SwapTotalBytes {
		return fmt.Errorf("el swap usado (%d) supera el total (%d)", m.SwapUsedBytes, m.SwapTotalBytes)
	}
	return nil
}

// LoadSample es la carga promedio de 1, 5 y 15 minutos.
type LoadSample struct {
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
}

// Validate revisa que las cargas no sean negativas.
func (l LoadSample) Validate() error {
	if l.Load1 < 0 || l.Load5 < 0 || l.Load15 < 0 {
		return fmt.Errorf("la carga no puede ser negativa: %v, %v, %v", l.Load1, l.Load5, l.Load15)
	}
	return nil
}

// DiskSample es el uso de un punto de montaje.
type DiskSample struct {
	Mountpoint string `json:"mountpoint"`
	TotalBytes uint64 `json:"total_bytes"`
	UsedBytes  uint64 `json:"used_bytes"`
}

// Validate revisa el nombre del montaje y que lo usado no supere lo total.
func (d DiskSample) Validate() error {
	switch {
	case d.Mountpoint == "":
		return errors.New("falta el punto de montaje")
	case len(d.Mountpoint) > MaxDeviceNameLen:
		return fmt.Errorf("el punto de montaje supera %d caracteres", MaxDeviceNameLen)
	case d.UsedBytes > d.TotalBytes:
		return fmt.Errorf("el espacio usado (%d) supera el total (%d)", d.UsedBytes, d.TotalBytes)
	}
	return nil
}

// NetworkSample son los contadores de una interfaz. Son acumulados desde que
// arrancó la máquina: la tasa se calcula en el control plane restando muestras,
// así el agent no guarda estado.
type NetworkSample struct {
	Interface string `json:"interface"`
	RxBytes   uint64 `json:"rx_bytes"`
	TxBytes   uint64 `json:"tx_bytes"`
}

// Validate revisa el nombre de la interfaz.
func (n NetworkSample) Validate() error {
	switch {
	case n.Interface == "":
		return errors.New("falta el nombre de la interfaz")
	case len(n.Interface) > MaxDeviceNameLen:
		return fmt.Errorf("el nombre de la interfaz supera %d caracteres", MaxDeviceNameLen)
	}
	return nil
}

// MetricsAck confirma hasta qué muestra se guardó. El agent usa LastTS para
// soltar del buffer lo que ya está a salvo y no reenviarlo.
type MetricsAck struct {
	Accepted int       `json:"accepted"`
	LastTS   time.Time `json:"last_ts"`
}

// Type implementa Message.
func (MetricsAck) Type() MessageType { return TypeMetricsAck }

// Validate implementa Message.
func (m MetricsAck) Validate() error {
	if m.Accepted < 0 {
		return errors.New("la cantidad aceptada no puede ser negativa")
	}
	if m.Accepted > 0 && m.LastTS.IsZero() {
		return errors.New("se aceptaron muestras pero no vino la marca de tiempo de la última")
	}
	return nil
}
