package collector

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strconv"
	"strings"
)

// cpuTimes son los contadores de la línea "cpu" de /proc/stat: cuánto tiempo
// pasaron todos los núcleos juntos ocupados y ociosos desde que arrancó la
// máquina, en ticks del kernel.
type cpuTimes struct {
	busy uint64
	idle uint64
}

// Posición de cada contador en la línea "cpu", en el orden en que los escribe
// el kernel (ver proc(5)). guest y guest_nice, que vienen después de steal,
// quedan afuera a propósito: el kernel ya los suma dentro de user y nice, y
// contarlos otra vez inflaría el uso.
const (
	fieldUser = iota
	fieldNice
	fieldSystem
	fieldIdle
	fieldIOWait
	fieldIRQ
	fieldSoftIRQ
	fieldSteal
	cpuFieldsUsed
)

// parseStat lee /proc/stat y devuelve los contadores de toda la máquina y la
// cantidad de núcleos en línea, que son las líneas cpu0, cpu1, etc.
func parseStat(raw string) (cpuTimes, int, error) {
	var (
		times cpuTimes
		found bool
		cores int
	)

	for line := range strings.Lines(raw) {
		name, rest, ok := strings.Cut(line, " ")
		if !ok || !strings.HasPrefix(name, "cpu") {
			continue
		}
		if name != "cpu" {
			if isDigits(name[len("cpu"):]) {
				cores++
			}
			continue
		}

		parsed, err := parseCPULine(rest)
		if err != nil {
			return cpuTimes{}, 0, fmt.Errorf("línea cpu de /proc/stat: %w", err)
		}
		times, found = parsed, true
	}

	if !found {
		return cpuTimes{}, 0, errors.New("/proc/stat no trae la línea cpu")
	}
	return times, cores, nil
}

// parseCPULine suma los contadores de una línea "cpu". Los kernels viejos traen
// menos campos (steal apareció en 2.6.11): los que faltan cuentan como cero,
// pero sin idle no hay forma de calcular nada.
func parseCPULine(rest string) (cpuTimes, error) {
	fields := strings.Fields(rest)
	if len(fields) <= fieldIdle {
		return cpuTimes{}, fmt.Errorf("trae %d contadores y hacen falta al menos %d", len(fields), fieldIdle+1)
	}

	var values [cpuFieldsUsed]uint64
	for i := range min(len(fields), cpuFieldsUsed) {
		value, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return cpuTimes{}, fmt.Errorf("contador %d (%q): %w", i, fields[i], err)
		}
		values[i] = value
	}

	busy, okBusy := sum(values[fieldUser], values[fieldNice], values[fieldSystem],
		values[fieldIRQ], values[fieldSoftIRQ], values[fieldSteal])
	idle, okIdle := sum(values[fieldIdle], values[fieldIOWait])
	_, okTotal := sum(busy, idle)
	if !okBusy || !okIdle || !okTotal {
		return cpuTimes{}, errors.New("los contadores no tienen sentido: la suma desborda")
	}

	return cpuTimes{busy: busy, idle: idle}, nil
}

// usedPercent calcula qué parte del tiempo entre dos lecturas estuvo ocupada,
// redondeada a dos decimales: el panel no muestra más y el JSON queda corto.
//
// Devuelve false si los contadores no avanzaron o retrocedieron. No pasa en un
// kernel normal con lecturas separadas por segundos, pero sí con /proc
// virtualizado (lxcfs) o con lecturas demasiado seguidas, y un porcentaje
// inventado sería peor que no tenerlo.
func usedPercent(prev, cur cpuTimes) (float64, bool) {
	prevTotal, curTotal := prev.busy+prev.idle, cur.busy+cur.idle
	if curTotal <= prevTotal {
		return 0, false
	}
	elapsed := curTotal - prevTotal

	// iowait puede retroceder (proc(5) lo advierte), así que la parte ociosa se
	// acota para que el resultado quede siempre entre 0 y 100.
	var idle uint64
	if cur.idle > prev.idle {
		idle = min(cur.idle-prev.idle, elapsed)
	}

	percent := float64(elapsed-idle) / float64(elapsed) * 100
	return math.Round(percent*100) / 100, true
}

// sum suma sin desbordar; el segundo valor es false si la suma no entra.
func sum(values ...uint64) (uint64, bool) {
	var total uint64
	for _, value := range values {
		var carry uint64
		total, carry = bits.Add64(total, value, 0)
		if carry != 0 {
			return 0, false
		}
	}
	return total, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
