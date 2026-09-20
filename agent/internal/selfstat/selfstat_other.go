//go:build !linux

package selfstat

import (
	"runtime"
	"time"
)

// readUsage fuera de Linux devuelve una aproximación, suficiente para
// desarrollar en Windows o macOS.
//
// El agent de verdad corre en Linux: ahí se leen /proc/self/status y
// /proc/self/stat, que dan la memoria residente real y el tiempo de CPU del
// proceso. Acá se informa la memoria que el runtime de Go pidió al sistema
// operativo, que es parecida pero no igual, y el tiempo de CPU queda en cero
// porque medirlo sin dependencias no es portable.
func readUsage() (usage, error) {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)

	return usage{
		memoryBytes: stats.Sys - stats.HeapReleased,
		cpuTime:     time.Duration(0),
	}, nil
}
