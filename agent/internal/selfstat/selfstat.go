// Package selfstat mide lo que consume el propio agent.
//
// Va en el latido porque es una de las metas del proyecto: menos de 30 MB de
// memoria y menos de 1% de CPU en reposo. Medirlo desde adentro permite verlo
// en el panel y en CI sin instalar nada aparte.
package selfstat

import (
	"sync"
	"time"
)

// Sample es el consumo del agent en un instante.
type Sample struct {
	// MemoryBytes es la memoria residente: lo que el proceso ocupa de verdad en
	// RAM, no lo que reservó.
	MemoryBytes uint64
	// CPUPercent es el uso de CPU desde la medición anterior, normalizado al
	// total de la máquina. La primera medición devuelve 0, porque no hay
	// intervalo contra el cual comparar.
	CPUPercent float64
}

// Sampler recuerda la medición anterior para poder calcular el uso de CPU, que
// es una diferencia entre dos instantes y no un valor puntual.
type Sampler struct {
	mu sync.Mutex

	lastCPUTime time.Duration
	lastWall    time.Time
	started     bool

	// now y readSelf se reemplazan en los tests.
	now      func() time.Time
	readSelf func() (usage, error)
}

// usage es la lectura cruda del sistema operativo.
type usage struct {
	memoryBytes uint64
	cpuTime     time.Duration
}

// NewSampler arma un medidor.
func NewSampler() *Sampler {
	return &Sampler{now: time.Now, readSelf: readUsage}
}

// Sample toma una medición. Es seguro llamarlo desde varias goroutines.
func (s *Sampler) Sample() (Sample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, err := s.read()
	if err != nil {
		return Sample{}, err
	}
	wall := s.clock()

	sample := Sample{MemoryBytes: current.memoryBytes}

	if s.started {
		elapsed := wall.Sub(s.lastWall)
		if elapsed > 0 {
			used := current.cpuTime - s.lastCPUTime
			sample.CPUPercent = percentOf(used, elapsed)
		}
	}

	s.lastCPUTime = current.cpuTime
	s.lastWall = wall
	s.started = true

	return sample, nil
}

// percentOf convierte tiempo de CPU consumido en un porcentaje del intervalo,
// acotado a [0,100] para que una medición rara no rompa la validación del
// mensaje del protocolo.
func percentOf(used, elapsed time.Duration) float64 {
	if elapsed <= 0 || used < 0 {
		return 0
	}
	percent := float64(used) / float64(elapsed) * 100
	return min(percent, 100)
}

func (s *Sampler) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Sampler) read() (usage, error) {
	if s.readSelf != nil {
		return s.readSelf()
	}
	return readUsage()
}
