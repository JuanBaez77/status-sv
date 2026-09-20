//go:build linux

package selfstat

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// readUsage lee /proc, que es la fuente barata y sin dependencias en Linux, que
// es donde corre el agent de verdad.
func readUsage() (usage, error) {
	memoryBytes, err := readResidentMemory()
	if err != nil {
		return usage{}, err
	}
	cpuTime, err := readCPUTime()
	if err != nil {
		return usage{}, err
	}
	return usage{memoryBytes: memoryBytes, cpuTime: cpuTime}, nil
}

// readResidentMemory saca VmRSS de /proc/self/status, que viene en kilobytes.
func readResidentMemory() (uint64, error) {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, fmt.Errorf("leer /proc/self/status: %w", err)
	}

	for line := range strings.Lines(string(raw)) {
		rest, found := strings.CutPrefix(line, "VmRSS:")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, fmt.Errorf("VmRSS sin valor: %q", strings.TrimSpace(line))
		}
		kilobytes, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("leer VmRSS (%q): %w", fields[0], err)
		}
		return kilobytes * 1024, nil
	}

	return 0, fmt.Errorf("no se encontró VmRSS en /proc/self/status")
}

// readCPUTime suma el tiempo de usuario y de sistema de /proc/self/stat.
//
// Los dos vienen en "ticks" del reloj del kernel, que en Linux moderno son
// siempre 100 por segundo para el espacio de usuario.
const ticksPerSecond = 100

func readCPUTime() (time.Duration, error) {
	raw, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, fmt.Errorf("leer /proc/self/stat: %w", err)
	}

	// El segundo campo es el nombre del ejecutable entre paréntesis y puede
	// tener espacios, así que se cuenta desde el último paréntesis.
	closing := strings.LastIndex(string(raw), ")")
	if closing < 0 {
		return 0, fmt.Errorf("/proc/self/stat tiene un formato inesperado")
	}

	// Después del paréntesis, el primer campo es el estado; utime y stime son
	// el 12.º y el 13.º de esa porción.
	fields := strings.Fields(string(raw)[closing+1:])
	const (
		utimeIndex = 11
		stimeIndex = 12
	)
	if len(fields) <= stimeIndex {
		return 0, fmt.Errorf("/proc/self/stat trae %d campos, se esperaban más", len(fields))
	}

	utime, err := strconv.ParseUint(fields[utimeIndex], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("leer utime: %w", err)
	}
	stime, err := strconv.ParseUint(fields[stimeIndex], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("leer stime: %w", err)
	}

	ticks := utime + stime
	return time.Duration(ticks) * time.Second / ticksPerSecond, nil
}
