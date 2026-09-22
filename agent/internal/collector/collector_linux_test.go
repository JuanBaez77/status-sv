//go:build linux

package collector

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// Contra el /proc de la máquina que corre el test: en el CI, un Ubuntu real.
// Lo que se verifica es que la muestra cumpla el contrato y tenga sentido; los
// valores exactos dependen de la máquina.
func TestCollectFromTheRealSystem(t *testing.T) {
	collector := New()
	ctx := context.Background()

	if _, err := collector.Collect(ctx); !errors.Is(err, ErrNoBaseline) {
		t.Fatalf("la primera lectura devolvió %v, se esperaba ErrNoBaseline", err)
	}

	// Entre dos lecturas muy seguidas los contadores pueden no avanzar: se
	// reintenta un rato antes de dar el test por fallido.
	deadline := time.Now().Add(5 * time.Second)
	for {
		time.Sleep(200 * time.Millisecond)
		sample, err := collector.Collect(ctx)
		if errors.Is(err, ErrNoBaseline) && time.Now().Before(deadline) {
			continue
		}

		var partial *PartialError
		if errors.As(err, &partial) {
			t.Logf("muestra incompleta (se acepta, depende de la máquina): %v", err)
		} else if err != nil {
			t.Fatalf("Collect: %v", err)
		}

		if err := sample.Validate(); err != nil {
			t.Errorf("la muestra no cumple el contrato: %v", err)
		}
		if sample.Memory.TotalBytes == 0 {
			t.Error("la memoria total dio cero")
		}
		// NumCPU cuenta los núcleos que el proceso puede usar, que son como
		// mucho los que están en línea.
		if sample.CPU.Cores < runtime.NumCPU() {
			t.Errorf("núcleos = %d, pero el proceso puede usar %d", sample.CPU.Cores, runtime.NumCPU())
		}
		for _, disk := range sample.Disks {
			if disk.TotalBytes == 0 {
				t.Errorf("el disco %q informa tamaño cero", disk.Mountpoint)
			}
		}

		t.Logf("CPU %.2f%% de %d núcleos, memoria %d/%d MiB, carga %.2f",
			sample.CPU.UsedPercent, sample.CPU.Cores,
			sample.Memory.UsedBytes>>20, sample.Memory.TotalBytes>>20, sample.Load.Load1)
		for _, disk := range sample.Disks {
			t.Logf("disco %q: %d/%d MiB", disk.Mountpoint, disk.UsedBytes>>20, disk.TotalBytes>>20)
		}
		for _, network := range sample.Networks {
			t.Logf("interfaz %q: rx %d, tx %d", network.Interface, network.RxBytes, network.TxBytes)
		}
		return
	}
}

func TestStatfsOnTheRoot(t *testing.T) {
	usage, err := statfs("/")
	if err != nil {
		t.Fatalf("statfs: %v", err)
	}
	if usage.total == 0 || usage.used > usage.total {
		t.Errorf("uso imposible de la raíz: %+v", usage)
	}
}

func TestStatfsReportsMissingPaths(t *testing.T) {
	if _, err := statfs("/no/existe/esta/ruta"); err == nil {
		t.Fatal("se esperaba un error")
	}
}

// BenchmarkCollectRealProc mide una muestra entera contra el /proc de verdad,
// con las llamadas al sistema incluidas.
func BenchmarkCollectRealProc(b *testing.B) {
	collector := New()
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		collector.prevCPU, collector.hasPrevCPU = cpuTimes{}, true
		if _, err := collector.Collect(ctx); err != nil {
			var partial *PartialError
			if !errors.As(err, &partial) {
				b.Fatal(err)
			}
		}
	}
}
