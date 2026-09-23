package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/collector"
	"github.com/JuanBaez77/status-sv/proto"
)

// Lo que representa cada agent simulado: un VPS chico, del tamaño de los que
// monitorea status-sv.
const (
	fakeCores    = 4
	fakeMemTotal = 8 << 30  // 8 GiB
	fakeSwap     = 2 << 30  // 2 GiB
	fakeDiskSize = 50 << 30 // 50 GiB por disco
)

// syntheticCollector inventa lecturas con la forma exacta de las de verdad: el
// control plane no puede notar la diferencia, así que la prueba mide la misma
// validación, la misma ingesta y el mismo agregado que un VPS real.
//
// Los números se mueven de una muestra a la otra —si fueran constantes, la
// compresión y la base trabajarían menos que en la realidad— y los contadores
// de red solo suben, como los del kernel.
type syntheticCollector struct {
	disks    int
	networks int

	mu       sync.Mutex
	rng      *rand.Rand
	calls    int
	produced int
	rx, tx   []uint64
	used     []uint64
}

func newSyntheticCollector(seed, disks, networks int) *syntheticCollector {
	c := &syntheticCollector{
		disks:    disks,
		networks: networks,
		//nolint:gosec // no es criptografía: son lecturas de mentira reproducibles
		rng:  rand.New(rand.NewPCG(uint64(seed), 0xC0FFEE)),
		rx:   make([]uint64, networks),
		tx:   make([]uint64, networks),
		used: make([]uint64, disks),
	}
	for i := range c.used {
		// Cada disco arranca entre el 30% y el 80% de ocupación.
		c.used[i] = uint64(float64(fakeDiskSize) * (0.3 + 0.5*c.rng.Float64()))
	}
	return c
}

// Collect implementa metrics.Collector.
func (c *syntheticCollector) Collect(_ context.Context) (proto.Sample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls++
	if c.calls == 1 {
		// Igual que el colector de verdad: la primera lectura solo deja la base
		// contra la cual calcular el CPU, y no produce muestra.
		return proto.Sample{}, collector.ErrNoBaseline
	}

	cpu := 5 + 55*c.rng.Float64()
	iowait := 3 * c.rng.Float64()
	memUsed := uint64(float64(fakeMemTotal) * (0.3 + 0.4*c.rng.Float64()))

	sample := proto.Sample{
		TS: time.Now().UTC(),
		CPU: proto.CPUSample{
			UsedPercent:   cpu,
			IOWaitPercent: iowait,
			StealPercent:  0,
			Cores:         fakeCores,
		},
		Memory: proto.MemorySample{
			TotalBytes:     fakeMemTotal,
			UsedBytes:      memUsed,
			SwapTotalBytes: fakeSwap,
			SwapUsedBytes:  fakeSwap / 20,
		},
		Load: proto.LoadSample{
			Load1:  cpu / 100 * fakeCores,
			Load5:  cpu / 120 * fakeCores,
			Load15: cpu / 150 * fakeCores,
		},
		Disks:    make([]proto.DiskSample, 0, c.disks),
		Networks: make([]proto.NetworkSample, 0, c.networks),
	}

	for i := range c.disks {
		// El disco crece de a poco, como en un servidor que acumula registros.
		c.used[i] = min(c.used[i]+c.rng.Uint64N(1<<20), fakeDiskSize)
		sample.Disks = append(sample.Disks, proto.DiskSample{
			Mountpoint:     mountpoint(i),
			TotalBytes:     fakeDiskSize,
			UsedBytes:      c.used[i],
			AvailableBytes: fakeDiskSize - c.used[i],
		})
	}

	for i := range c.networks {
		// Los contadores del kernel son acumulados: solo suben.
		c.rx[i] += c.rng.Uint64N(2 << 20)
		c.tx[i] += c.rng.Uint64N(1 << 20)
		sample.Networks = append(sample.Networks, proto.NetworkSample{
			Interface: iface(i),
			RxBytes:   c.rx[i],
			TxBytes:   c.tx[i],
		})
	}

	c.produced++
	return sample, nil
}

// collected es cuántas muestras entregó, sin contar la lectura de base.
func (c *syntheticCollector) collected() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.produced
}

func mountpoint(i int) string {
	if i == 0 {
		return "/"
	}
	return fmt.Sprintf("/mnt/datos%d", i)
}

func iface(i int) string {
	return fmt.Sprintf("eth%d", i)
}
