package collector

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/JuanBaez77/status-sv/proto"
)

// parseMeminfo lee /proc/meminfo.
//
// La memoria usada es la total menos MemAvailable, que es lo que el kernel
// estima que se puede usar sin mandar nada a swap: la caché de disco que el
// kernel suelta solo no cuenta como usada. Es el mismo criterio que la columna
// "available" de free, y el único que no hace parecer lleno a un servidor sano.
func parseMeminfo(raw string) (proto.MemorySample, error) {
	var (
		total, available, free, buffers, cached, swapTotal, swapFree uint64

		haveTotal, haveAvailable bool
	)

	for line := range strings.Lines(raw) {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}

		var target *uint64
		switch key {
		case "MemTotal":
			target, haveTotal = &total, true
		case "MemAvailable":
			target, haveAvailable = &available, true
		case "MemFree":
			target = &free
		case "Buffers":
			target = &buffers
		case "Cached":
			target = &cached
		case "SwapTotal":
			target = &swapTotal
		case "SwapFree":
			target = &swapFree
		default:
			continue
		}

		value, err := parseKilobytes(rest)
		if err != nil {
			return proto.MemorySample{}, fmt.Errorf("%s de /proc/meminfo: %w", key, err)
		}
		*target = value
	}

	if !haveTotal {
		return proto.MemorySample{}, errors.New("/proc/meminfo no trae MemTotal")
	}
	if !haveAvailable {
		// Los kernels anteriores a 3.14 no tienen MemAvailable. La aproximación
		// de siempre es lo libre más las cachés que el kernel puede soltar.
		available = free + buffers + cached
	}

	// Los min cubren un /proc/meminfo virtualizado que informe más disponible
	// que total: la muestra tiene que cumplir el contrato igual.
	return proto.MemorySample{
		TotalBytes:     total,
		UsedBytes:      total - min(available, total),
		SwapTotalBytes: swapTotal,
		SwapUsedBytes:  swapTotal - min(swapFree, swapTotal),
	}, nil
}

// parseKilobytes lee un valor como "  8104672 kB" y lo pasa a bytes. El kernel
// escribe "kB" pero son kibibytes: multiplica por 1024.
func parseKilobytes(rest string) (uint64, error) {
	fields := strings.Fields(rest)
	if len(fields) != 2 || fields[1] != "kB" {
		return 0, fmt.Errorf("formato inesperado: %q", strings.TrimSpace(rest))
	}
	kilobytes, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("leer %q: %w", fields[0], err)
	}
	if kilobytes > math.MaxUint64/1024 {
		return 0, fmt.Errorf("valor fuera de rango: %d kB", kilobytes)
	}
	return kilobytes * 1024, nil
}

// parseLoadavg lee los tres promedios de carga de /proc/loadavg, que empieza
// así: "0.35 0.14 0.10 7/384 10".
func parseLoadavg(raw string) (proto.LoadSample, error) {
	fields := strings.Fields(raw)
	if len(fields) < 3 {
		return proto.LoadSample{}, fmt.Errorf("/proc/loadavg tiene un formato inesperado: %q", strings.TrimSpace(raw))
	}

	var values [3]float64
	for i := range values {
		value, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return proto.LoadSample{}, fmt.Errorf("leer la carga %q de /proc/loadavg: %w", fields[i], err)
		}
		// ParseFloat acepta "NaN" e "Inf", que después no se pueden serializar
		// en JSON; el kernel no los escribe, pero la frontera se valida igual.
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return proto.LoadSample{}, fmt.Errorf("carga imposible en /proc/loadavg: %q", fields[i])
		}
		values[i] = value
	}

	return proto.LoadSample{Load1: values[0], Load5: values[1], Load15: values[2]}, nil
}
