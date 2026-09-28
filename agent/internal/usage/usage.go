// Package usage convierte tiempo de CPU acumulado en un porcentaje del total de
// la máquina, comparando cada lectura con la anterior de la misma unidad.
//
// Sirve para todo lo que el kernel cuenta como tiempo acumulado: el cpu.stat de
// un cgroup (contenedores y servicios) y el utime + stime de un proceso.
package usage

import (
	"math"
	"time"
)

// Tracker recuerda la última lectura de cada unidad.
//
// No es seguro usarlo desde varias goroutines: el agent arma el estado de
// runtime desde un solo lugar.
type Tracker struct {
	cores int
	last  map[string]reading
	seen  map[string]bool
}

type reading struct {
	cpu time.Duration
	at  time.Time
}

// NewTracker arma un Tracker para una máquina con cores núcleos. El porcentaje
// se normaliza al total, igual que el CPU del servidor: un proceso que ocupa un
// núcleo entero de cuatro marca 25.
func NewTracker(cores int) *Tracker {
	return &Tracker{
		cores: max(cores, 1),
		last:  make(map[string]reading),
		seen:  make(map[string]bool),
	}
}

// CPUPercent registra que la unidad key llevaba cpu de tiempo de CPU en el
// instante at, y devuelve el porcentaje desde la lectura anterior, con dos
// decimales.
//
// Devuelve false cuando todavía no hay contra qué comparar: la primera vez que
// aparece la unidad, o cuando su contador volvió atrás porque se recreó (un
// contenedor reiniciado con el mismo ID, un servicio que se volvió a crear).
func (t *Tracker) CPUPercent(key string, cpu time.Duration, at time.Time) (float64, bool) {
	t.seen[key] = true
	prev, ok := t.last[key]
	t.last[key] = reading{cpu: cpu, at: at}

	elapsed := at.Sub(prev.at)
	if !ok || elapsed <= 0 || cpu < prev.cpu {
		return 0, false
	}

	percent := float64(cpu-prev.cpu) / (float64(elapsed) * float64(t.cores)) * 100
	// Dos decimales alcanzan para un panel, y un número como 0.0012963399449264
	// solo agranda el mensaje.
	percent = math.Round(percent*100) / 100
	return min(percent, 100), true
}

// Sweep olvida las unidades que no se leyeron desde el Sweep anterior. Se
// llama una vez por ciclo, así un contenedor borrado no queda para siempre en
// la memoria del agent.
func (t *Tracker) Sweep() {
	for key := range t.last {
		if !t.seen[key] {
			delete(t.last, key)
		}
	}
	clear(t.seen)
}

// Len dice cuántas unidades recuerda. Es para los tests y para vigilar que la
// memoria del agent no crezca.
func (t *Tracker) Len() int {
	return len(t.last)
}
