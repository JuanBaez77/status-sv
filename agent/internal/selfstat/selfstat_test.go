package selfstat

import (
	"errors"
	"testing"
	"time"
)

// fakeSampler devuelve lecturas controladas, para poder probar el cálculo del
// porcentaje sin depender de lo que haga el proceso de verdad.
func fakeSampler(readings []usage, clock []time.Time) *Sampler {
	index := 0
	tick := 0

	return &Sampler{
		now: func() time.Time {
			value := clock[min(tick, len(clock)-1)]
			tick++
			return value
		},
		readSelf: func() (usage, error) {
			value := readings[min(index, len(readings)-1)]
			index++
			return value, nil
		},
	}
}

// La primera medición no tiene con qué comparar, así que el CPU es cero.
func TestFirstSampleHasNoCPU(t *testing.T) {
	start := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	sampler := fakeSampler(
		[]usage{{memoryBytes: 20 << 20, cpuTime: 5 * time.Second}},
		[]time.Time{start},
	)

	sample, err := sampler.Sample()
	if err != nil {
		t.Fatalf("medir: %v", err)
	}
	if sample.CPUPercent != 0 {
		t.Errorf("la primera medición informó %v%% de CPU", sample.CPUPercent)
	}
	if sample.MemoryBytes != 20<<20 {
		t.Errorf("memoria = %d", sample.MemoryBytes)
	}
}

func TestCPUPercentIsTheShareOfTheInterval(t *testing.T) {
	start := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		cpuUsed  time.Duration
		interval time.Duration
		want     float64
	}{
		{name: "en reposo", cpuUsed: 10 * time.Millisecond, interval: 10 * time.Second, want: 0.1},
		{name: "un décimo", cpuUsed: time.Second, interval: 10 * time.Second, want: 10},
		{name: "un núcleo entero", cpuUsed: 10 * time.Second, interval: 10 * time.Second, want: 100},
		{name: "más de un núcleo se recorta", cpuUsed: 30 * time.Second, interval: 10 * time.Second, want: 100},
		{name: "sin consumo", cpuUsed: 0, interval: 10 * time.Second, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sampler := fakeSampler(
				[]usage{
					{memoryBytes: 20 << 20, cpuTime: 5 * time.Second},
					{memoryBytes: 21 << 20, cpuTime: 5*time.Second + tt.cpuUsed},
				},
				[]time.Time{start, start.Add(tt.interval)},
			)

			if _, err := sampler.Sample(); err != nil {
				t.Fatalf("primera medición: %v", err)
			}
			second, err := sampler.Sample()
			if err != nil {
				t.Fatalf("segunda medición: %v", err)
			}

			if second.CPUPercent != tt.want {
				t.Errorf("CPU = %v%%, se esperaba %v%%", second.CPUPercent, tt.want)
			}
		})
	}
}

// El reloj del sistema puede saltar hacia atrás; eso no puede producir un
// porcentaje negativo, que rompería la validación del latido.
func TestClockGoingBackwardsDoesNotBreakTheSample(t *testing.T) {
	start := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	sampler := fakeSampler(
		[]usage{
			{memoryBytes: 20 << 20, cpuTime: 5 * time.Second},
			{memoryBytes: 20 << 20, cpuTime: 6 * time.Second},
		},
		[]time.Time{start, start.Add(-time.Minute)},
	)

	if _, err := sampler.Sample(); err != nil {
		t.Fatalf("primera medición: %v", err)
	}
	second, err := sampler.Sample()
	if err != nil {
		t.Fatalf("segunda medición: %v", err)
	}

	if second.CPUPercent < 0 || second.CPUPercent > 100 {
		t.Errorf("CPU fuera de rango: %v", second.CPUPercent)
	}
}

func TestSampleReportsReadErrors(t *testing.T) {
	sampler := &Sampler{
		now:      time.Now,
		readSelf: func() (usage, error) { return usage{}, errors.New("falló la lectura") },
	}

	if _, err := sampler.Sample(); err == nil {
		t.Fatal("se esperaba un error de lectura")
	}
}

// Contra el sistema operativo de verdad: cualquiera sea la plataforma, la
// medición tiene que devolver algo coherente con el contrato del latido.
func TestRealSampleIsWithinProtocolRange(t *testing.T) {
	sampler := NewSampler()

	first, err := sampler.Sample()
	if err != nil {
		t.Fatalf("medir: %v", err)
	}
	if first.MemoryBytes == 0 {
		t.Error("la memoria medida dio cero")
	}

	// Un poco de trabajo para que haya algo que medir entre las dos lecturas.
	suma := 0
	for i := range 2_000_000 {
		suma += i
	}
	_ = suma

	second, err := sampler.Sample()
	if err != nil {
		t.Fatalf("segunda medición: %v", err)
	}
	if second.CPUPercent < 0 || second.CPUPercent > 100 {
		t.Errorf("el CPU quedó fuera del rango que acepta el protocolo: %v", second.CPUPercent)
	}
}
