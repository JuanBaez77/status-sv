// Package collector lee el estado del servidor desde /proc: CPU, memoria,
// carga, discos e interfaces de red.
//
// Lee /proc directamente, sin dependencias ni procesos externos, porque es lo
// más barato que hay en Linux y el agent tiene que quedar por debajo del 1% de
// CPU. Cada muestra son cinco archivos chicos y un statfs por disco.
package collector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/JuanBaez77/status-sv/proto"
)

// ErrNoBaseline indica que la lectura no trae muestra. El uso de CPU es una
// diferencia entre dos lecturas de /proc/stat, y esta fue la primera (o los
// contadores no avanzaron y hubo que empezar de nuevo). Quien llama descarta la
// lectura y espera al intervalo siguiente; la próxima ya trae muestra.
var ErrNoBaseline = errors.New("todavía no hay una lectura de CPU anterior contra la cual comparar")

// PartialError indica que la muestra se armó pero le falta algo: un disco que
// no se pudo medir o las interfaces de red.
//
// La muestra que acompaña a este error es válida y conviene mandarla igual:
// perder el CPU y la memoria por un disco que no responde sería peor.
type PartialError struct {
	Problems []error
}

func (e *PartialError) Error() string {
	messages := make([]string, len(e.Problems))
	for i, problem := range e.Problems {
		messages[i] = problem.Error()
	}
	return "muestra incompleta: " + strings.Join(messages, "; ")
}

// Unwrap permite usar errors.Is y errors.As sobre cada problema.
func (e *PartialError) Unwrap() []error {
	return e.Problems
}

// Collector toma muestras del servidor.
//
// Recuerda la lectura anterior de CPU, así que no es seguro usarlo desde varias
// goroutines a la vez: el agent lo llama desde un solo lugar.
type Collector struct {
	// proc es /proc. En los tests es un sistema de archivos en memoria con
	// archivos de muestra.
	proc fs.FS
	// statfs mide un disco. Los tests lo reemplazan, porque las rutas de los
	// archivos de muestra no existen en la máquina que corre el test.
	statfs func(path string) (fsUsage, error)
	now    func() time.Time

	prevCPU    cpuTimes
	hasPrevCPU bool
}

// New arma un colector que lee el /proc de la máquina.
func New() *Collector {
	return &Collector{
		proc:   os.DirFS("/proc"),
		statfs: statfs,
		now:    time.Now,
	}
}

// Collect toma una muestra.
//
// Devuelve ErrNoBaseline en la primera llamada, que solo guarda la lectura de
// CPU contra la cual comparar. Si la muestra salió sin algún disco o sin las
// interfaces, devuelve la muestra junto con un *PartialError. Cualquier otro
// error significa que no hay muestra: sin CPU, memoria o carga no tiene sentido
// mandarla.
func (c *Collector) Collect(ctx context.Context) (proto.Sample, error) {
	if err := ctx.Err(); err != nil {
		return proto.Sample{}, err
	}

	// Se trunca al milisegundo porque la marca de tiempo identifica a la
	// muestra de punta a punta: PostgreSQL guarda microsegundos, y con los
	// nanosegundos de Go la marca que vuelve en metrics_ack no coincidiría con
	// la que el agent tiene en su buffer.
	ts := c.now().UTC().Truncate(time.Millisecond)

	stat, err := c.read("stat")
	if err != nil {
		return proto.Sample{}, err
	}
	times, cores, err := parseStat(stat)
	if err != nil {
		return proto.Sample{}, err
	}
	cpu, err := c.cpuUsage(times)
	if err != nil {
		return proto.Sample{}, err
	}

	meminfo, err := c.read("meminfo")
	if err != nil {
		return proto.Sample{}, err
	}
	memory, err := parseMeminfo(meminfo)
	if err != nil {
		return proto.Sample{}, err
	}

	loadavg, err := c.read("loadavg")
	if err != nil {
		return proto.Sample{}, err
	}
	load, err := parseLoadavg(loadavg)
	if err != nil {
		return proto.Sample{}, err
	}

	sample := proto.Sample{
		TS: ts,
		CPU: proto.CPUSample{
			UsedPercent:   cpu.used,
			IOWaitPercent: cpu.iowait,
			StealPercent:  cpu.steal,
			Cores:         cores,
		},
		Memory: memory,
		Load:   load,
	}

	disks, diskProblems := c.disks(ctx)
	if err := ctx.Err(); err != nil {
		return proto.Sample{}, err
	}
	networks, networkProblems := c.networks()
	sample.Disks, sample.Networks = disks, networks

	// El colector recorta todo a los límites del contrato, así que esto no
	// debería fallar nunca. Si falla es un bug, y es mejor verlo acá que como
	// un lote entero rechazado por el control plane.
	if err := sample.Validate(); err != nil {
		return proto.Sample{}, fmt.Errorf("la muestra armada no cumple el contrato: %w", err)
	}

	if problems := append(diskProblems, networkProblems...); len(problems) > 0 {
		return sample, &PartialError{Problems: problems}
	}
	return sample, nil
}

// cpuUsage compara con la lectura anterior y la reemplaza por la actual.
func (c *Collector) cpuUsage(times cpuTimes) (cpuUsage, error) {
	prev, hadPrev := c.prevCPU, c.hasPrevCPU
	c.prevCPU, c.hasPrevCPU = times, true

	if !hadPrev {
		return cpuUsage{}, ErrNoBaseline
	}
	result, ok := usage(prev, times)
	if !ok {
		return cpuUsage{}, fmt.Errorf("%w: los contadores de /proc/stat no avanzaron", ErrNoBaseline)
	}
	return result, nil
}

// disks mide los discos que elige selectDisks. Un disco que falla no tira abajo
// a los demás: queda anotado como problema.
func (c *Collector) disks(ctx context.Context) ([]proto.DiskSample, []error) {
	raw, err := c.read("self/mountinfo")
	if err != nil {
		return nil, []error{err}
	}
	mounts, err := parseMountinfo(raw)
	if err != nil {
		return nil, []error{err}
	}

	candidates := selectDisks(mounts)
	if len(candidates) == 0 {
		// En un servidor de verdad siempre está al menos la raíz. Si no aparece
		// nada, lo más probable es un sistema de archivos que isDiskFilesystem
		// no conoce, y conviene que se vea en el registro.
		return nil, []error{errors.New("ningún montaje de /proc/self/mountinfo es un disco local conocido")}
	}

	var (
		disks    []proto.DiskSample
		problems []error
	)
	for _, m := range candidates {
		if ctx.Err() != nil {
			break
		}
		if len(disks) == proto.MaxDisksPerSample {
			problems = append(problems, fmt.Errorf("hay más de %d discos: se informan los primeros en orden alfabético", proto.MaxDisksPerSample))
			break
		}
		if len(m.point) > proto.MaxDeviceNameLen {
			problems = append(problems, fmt.Errorf("un punto de montaje de %d caracteres supera el máximo de %d y no se informa (empieza con %.40q)",
				len(m.point), proto.MaxDeviceNameLen, m.point))
			continue
		}

		space, err := c.statfs(m.point)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		disks = append(disks, proto.DiskSample{
			Mountpoint:     m.point,
			TotalBytes:     space.total,
			UsedBytes:      space.used,
			AvailableBytes: space.available,
		})
	}
	return disks, problems
}

// networks lee las interfaces de red, recortadas al límite del contrato.
func (c *Collector) networks() ([]proto.NetworkSample, []error) {
	raw, err := c.read("net/dev")
	if err != nil {
		return nil, []error{err}
	}
	networks, err := parseNetDev(raw)
	if err != nil {
		return nil, []error{fmt.Errorf("/proc/net/dev: %w", err)}
	}

	if len(networks) > proto.MaxNetworksPerSample {
		return networks[:proto.MaxNetworksPerSample], []error{
			fmt.Errorf("hay %d interfaces: se informan las primeras %d en orden alfabético", len(networks), proto.MaxNetworksPerSample),
		}
	}
	return networks, nil
}

// read lee un archivo de /proc entero. Los archivos de /proc informan tamaño
// cero, así que se leen hasta el final y no según su tamaño.
func (c *Collector) read(name string) (string, error) {
	raw, err := fs.ReadFile(c.proc, name)
	if err != nil {
		return "", fmt.Errorf("leer /proc/%s: %w", name, err)
	}
	return string(raw), nil
}
