package metrics

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/collector"
	"github.com/JuanBaez77/status-sv/proto"
)

// scriptedCollector devuelve una lista fija de resultados, y después
// ErrNoBaseline para siempre.
type scriptedCollector struct {
	mu      sync.Mutex
	results []result
	calls   int
}

type result struct {
	sample proto.Sample
	err    error
}

func (c *scriptedCollector) Collect(context.Context) (proto.Sample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls++
	if len(c.results) == 0 {
		return proto.Sample{}, collector.ErrNoBaseline
	}
	next := c.results[0]
	c.results = c.results[1:]
	return next.sample, next.err
}

func (c *scriptedCollector) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// recordingHandler guarda los registros para poder contar los avisos.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler            { return h }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *recordingHandler) count(level slog.Level, message string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	n := 0
	for _, r := range h.records {
		if r.Level == level && r.Message == message {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("se agotó el tiempo esperando: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func every(d time.Duration) func() time.Duration {
	return func() time.Duration { return d }
}

func TestSamplerStoresSamplesAndSurvivesFailures(t *testing.T) {
	errDisk := errors.New("disco /boot: error de entrada/salida")
	fake := &scriptedCollector{results: []result{
		{err: collector.ErrNoBaseline},
		{sample: sampleAt(1)},
		{sample: sampleAt(2), err: &collector.PartialError{Problems: []error{errDisk}}},
		{err: errors.New("leer /proc/meminfo: no existe")},
		{sample: sampleAt(4)},
	}}
	buffer := NewBuffer(DefaultMaxAge, DefaultMaxSamples)
	handler := &recordingHandler{}
	sampler := NewSampler(fake, buffer, every(time.Millisecond), slog.New(handler))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { sampler.Run(ctx); close(done) }()

	waitFor(t, "las tres muestras válidas", func() bool { return buffer.Len() == 3 })
	cancel()
	<-done

	// La incompleta se guarda; la que falló entera, no.
	batch := buffer.Next(10, t0)
	for i, want := range []int{1, 2, 4} {
		if !batch.Samples[i].TS.Equal(sampleAt(want).TS) {
			t.Errorf("muestra %d: %v, se esperaba la %d", i, batch.Samples[i].TS, want)
		}
	}
	if n := handler.count(slog.LevelWarn, "problema al tomar la muestra"); n != 2 {
		t.Errorf("hubo %d avisos de problemas, se esperaban 2", n)
	}
	if n := handler.count(slog.LevelInfo, "las muestras vuelven a salir completas"); n != 1 {
		t.Errorf("hubo %d avisos de recuperación, se esperaba 1", n)
	}
}

// El mismo problema en cada muestra no puede llenar el registro de avisos.
func TestSamplerDoesNotRepeatTheSameWarning(t *testing.T) {
	partial := &collector.PartialError{Problems: []error{errors.New("disco /srv: permiso denegado")}}
	results := make([]result, 20)
	for i := range results {
		results[i] = result{sample: sampleAt(i), err: partial}
	}
	fake := &scriptedCollector{results: results}
	buffer := NewBuffer(DefaultMaxAge, DefaultMaxSamples)
	handler := &recordingHandler{}
	sampler := NewSampler(fake, buffer, every(time.Millisecond), slog.New(handler))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { sampler.Run(ctx); close(done) }()

	waitFor(t, "las 20 muestras", func() bool { return buffer.Len() == 20 })
	cancel()
	<-done

	if n := handler.count(slog.LevelWarn, "problema al tomar la muestra"); n != 1 {
		t.Errorf("el mismo problema se avisó %d veces", n)
	}
}

func TestSamplerWarnsOnceWhenTheBufferOverflows(t *testing.T) {
	results := make([]result, 10)
	for i := range results {
		results[i] = result{sample: sampleAt(i)}
	}
	fake := &scriptedCollector{results: results}
	buffer := NewBuffer(DefaultMaxAge, 3)
	handler := &recordingHandler{}
	sampler := NewSampler(fake, buffer, every(time.Millisecond), slog.New(handler))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { sampler.Run(ctx); close(done) }()

	waitFor(t, "las 10 muestras", func() bool { return buffer.Dropped() == 7 })
	cancel()
	<-done

	const message = "el buffer de métricas está lleno: se descartan las muestras más viejas hasta que vuelva el canal"
	if n := handler.count(slog.LevelWarn, message); n != 1 {
		t.Errorf("el buffer lleno se avisó %d veces, se esperaba 1", n)
	}
}

// El control plane puede cambiar el intervalo: el muestreo lo toma en la
// vuelta siguiente.
func TestSamplerFollowsIntervalChanges(t *testing.T) {
	var interval atomic.Int64
	interval.Store(int64(time.Millisecond))

	fake := &scriptedCollector{}
	sampler := NewSampler(fake, NewBuffer(DefaultMaxAge, DefaultMaxSamples),
		func() time.Duration { return time.Duration(interval.Load()) },
		slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { sampler.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor(t, "varias lecturas rápidas", func() bool { return fake.callCount() >= 5 })
	interval.Store(int64(time.Hour))

	// Una vuelta más con el intervalo viejo, y después se queda quieto.
	time.Sleep(50 * time.Millisecond)
	before := fake.callCount()
	time.Sleep(100 * time.Millisecond)
	if after := fake.callCount(); after != before {
		t.Errorf("siguió leyendo cada milisegundo: %d lecturas más", after-before)
	}
}

func TestSamplerStopsWhenCanceled(t *testing.T) {
	sampler := NewSampler(&scriptedCollector{}, NewBuffer(DefaultMaxAge, DefaultMaxSamples),
		every(time.Hour), slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { sampler.Run(ctx); close(done) }()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("el muestreo no se detuvo al cancelar")
	}
}
