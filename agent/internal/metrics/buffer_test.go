package metrics

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

var t0 = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

// sampleAt arma una muestra válida a i intervalos de 10 s de t0.
func sampleAt(i int) proto.Sample {
	return proto.Sample{
		TS:     t0.Add(time.Duration(i) * 10 * time.Second),
		CPU:    proto.CPUSample{UsedPercent: float64(i % 100), Cores: 2},
		Memory: proto.MemorySample{TotalBytes: 100, UsedBytes: 50},
	}
}

func fill(b *Buffer, from, to int) {
	for i := from; i < to; i++ {
		b.Add(sampleAt(i))
	}
}

func timestamps(samples []proto.Sample) []time.Time {
	ts := make([]time.Time, len(samples))
	for i, s := range samples {
		ts[i] = s.TS
	}
	return ts
}

func assertBatch(t *testing.T, batch Batch, from, to int) {
	t.Helper()
	if len(batch.Samples) != to-from {
		t.Fatalf("el lote trae %d muestras, se esperaban %d (de la %d a la %d)", len(batch.Samples), to-from, from, to-1)
	}
	for i, s := range batch.Samples {
		if want := sampleAt(from + i).TS; !s.TS.Equal(want) {
			t.Fatalf("muestra %d del lote: %v, se esperaba %v\nlote: %v", i, s.TS, want, timestamps(batch.Samples))
		}
	}
}

// Con muestras cada 10 s, el buffer guarda 10 minutos: la muestra más nueva y
// las 60 anteriores.
func TestAddKeepsTenMinutes(t *testing.T) {
	b := NewBuffer(DefaultMaxAge, DefaultMaxSamples)
	fill(b, 0, 100)

	if got := b.Len(); got != 61 {
		t.Errorf("guarda %d muestras, se esperaban 61", got)
	}
	if got := b.Dropped(); got != 39 {
		t.Errorf("descartó %d, se esperaban 39", got)
	}
	assertBatch(t, b.Next(1000, t0), 39, 100)
}

func TestAddCapsTheNumberOfSamples(t *testing.T) {
	b := NewBuffer(time.Hour, 3)

	for i := range 5 {
		want := 0
		if i >= 3 {
			want = 1
		}
		if dropped := b.Add(sampleAt(i)); dropped != want {
			t.Errorf("al agregar la muestra %d descartó %d, se esperaba %d", i, dropped, want)
		}
	}
	assertBatch(t, b.Next(10, t0), 2, 5)
}

func TestNextGivesTheOldestFirstUpToTheLimit(t *testing.T) {
	b := NewBuffer(DefaultMaxAge, DefaultMaxSamples)
	if batch := b.Next(10, t0); len(batch.Samples) != 0 {
		t.Fatalf("con el buffer vacío salió un lote de %d", len(batch.Samples))
	}

	fill(b, 0, 5)
	assertBatch(t, b.Next(3, t0), 0, 3)
	// Next no saca nada: sin Sent, el lote no está en vuelo.
	assertBatch(t, b.Next(10, t0), 0, 5)
}

func TestOnlyOneBatchIsInFlight(t *testing.T) {
	b := NewBuffer(DefaultMaxAge, DefaultMaxSamples)
	fill(b, 0, 3)

	batch := b.Next(10, t0)
	b.Sent(batch, len(batch.Samples), uuid.New(), t0)
	fill(b, 3, 4)

	if again := b.Next(10, t0.Add(time.Second)); len(again.Samples) != 0 {
		t.Fatalf("salió otro lote con uno en vuelo: %v", timestamps(again.Samples))
	}

	if released := b.Ack(sampleAt(2).TS); released != 3 {
		t.Errorf("el acuse soltó %d muestras, se esperaban 3", released)
	}
	assertBatch(t, b.Next(10, t0.Add(time.Second)), 3, 4)
}

// El acuse confirma hasta una marca de tiempo: lo que quedó del lote sin
// confirmar vuelve a salir en el lote siguiente.
func TestPartialAckResendsTheRest(t *testing.T) {
	b := NewBuffer(DefaultMaxAge, DefaultMaxSamples)
	fill(b, 0, 4)

	batch := b.Next(10, t0)
	b.Sent(batch, 4, uuid.New(), t0)

	if released := b.Ack(sampleAt(1).TS); released != 2 {
		t.Errorf("soltó %d, se esperaban 2", released)
	}
	assertBatch(t, b.Next(10, t0), 2, 4)
}

// Si el lote no entraba entero en un mensaje, solo se mandó una parte: el
// acuse no puede soltar lo que no salió.
func TestAckNeverReleasesWhatWasNotSent(t *testing.T) {
	b := NewBuffer(DefaultMaxAge, DefaultMaxSamples)
	fill(b, 0, 5)

	batch := b.Next(10, t0)
	b.Sent(batch, 2, uuid.New(), t0)

	// Un acuse con una marca posterior a todo (un control plane con errores,
	// o un reloj que saltó) suelta solo las dos que salieron.
	if released := b.Ack(sampleAt(100).TS); released != 2 {
		t.Errorf("soltó %d, se esperaban las 2 que salieron", released)
	}
	assertBatch(t, b.Next(10, t0), 2, 5)
}

// Si el reloj salta hacia atrás, una muestra nueva puede tener una marca
// anterior a otras ya enviadas. El acuse de esas no la puede soltar.
func TestAckWithClockGoingBackwards(t *testing.T) {
	b := NewBuffer(DefaultMaxAge, DefaultMaxSamples)
	b.Add(sampleAt(10))

	batch := b.Next(10, t0)
	b.Sent(batch, 1, uuid.New(), t0)
	b.Add(sampleAt(5))

	if released := b.Ack(sampleAt(10).TS); released != 1 {
		t.Errorf("soltó %d, se esperaba 1", released)
	}
	if b.Len() != 1 {
		t.Errorf("se perdió la muestra que no había salido")
	}
}

func TestLostBatchIsResentAfterTheTimeout(t *testing.T) {
	b := NewBuffer(DefaultMaxAge, DefaultMaxSamples)
	fill(b, 0, 2)

	batch := b.Next(10, t0)
	b.Sent(batch, 2, uuid.New(), t0)

	if again := b.Next(10, t0.Add(DefaultAckTimeout-time.Second)); len(again.Samples) != 0 {
		t.Fatal("se reenvió antes del plazo")
	}
	assertBatch(t, b.Next(10, t0.Add(DefaultAckTimeout)), 0, 2)
}

func TestRestartResendsTheBatchInFlight(t *testing.T) {
	b := NewBuffer(DefaultMaxAge, DefaultMaxSamples)
	fill(b, 0, 2)

	batch := b.Next(10, t0)
	b.Sent(batch, 2, uuid.New(), t0)
	b.Restart()

	assertBatch(t, b.Next(10, t0), 0, 2)
}

func TestReject(t *testing.T) {
	tests := []struct {
		name          string
		retryable     bool
		wantDiscarded int
		wantFrom      int
	}{
		// El control plane está saturado: las muestras vuelven a salir.
		{name: "reintentable", retryable: true, wantDiscarded: 0, wantFrom: 0},
		// No las va a aceptar nunca: se descartan las que salieron, y solo esas.
		{name: "definitivo", retryable: false, wantDiscarded: 3, wantFrom: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBuffer(DefaultMaxAge, DefaultMaxSamples)
			fill(b, 0, 5)
			id := uuid.New()

			batch := b.Next(10, t0)
			b.Sent(batch, 3, id, t0)

			discarded, matched := b.Reject(id, tt.retryable)
			if !matched {
				t.Fatal("el rechazo no se asoció al lote en vuelo")
			}
			if discarded != tt.wantDiscarded {
				t.Errorf("descartó %d, se esperaban %d", discarded, tt.wantDiscarded)
			}
			assertBatch(t, b.Next(10, t0), tt.wantFrom, 5)
		})
	}
}

func TestRejectOfAnotherMessageIsIgnored(t *testing.T) {
	b := NewBuffer(DefaultMaxAge, DefaultMaxSamples)
	fill(b, 0, 2)

	batch := b.Next(10, t0)
	b.Sent(batch, 2, uuid.New(), t0)

	if _, matched := b.Reject(uuid.New(), false); matched {
		t.Fatal("se asoció un rechazo de otro mensaje al lote en vuelo")
	}
	if b.Len() != 2 {
		t.Errorf("un rechazo ajeno descartó muestras")
	}
	if again := b.Next(10, t0); len(again.Samples) != 0 {
		t.Error("un rechazo ajeno liberó el lote en vuelo")
	}
}

// Si el buffer se llena mientras un lote está en vuelo, lo más viejo se
// descarta igual, y el acuse posterior no se confunde.
func TestDroppingWhileABatchIsInFlight(t *testing.T) {
	b := NewBuffer(time.Hour, 3)
	fill(b, 0, 3)

	batch := b.Next(10, t0)
	b.Sent(batch, 3, uuid.New(), t0)
	fill(b, 3, 4) // descarta la 0, que estaba en vuelo

	if released := b.Ack(sampleAt(2).TS); released != 2 {
		t.Errorf("soltó %d, se esperaban 2 (la 1 y la 2)", released)
	}
	assertBatch(t, b.Next(10, t0), 3, 4)
}

func TestDiscardOldest(t *testing.T) {
	b := NewBuffer(DefaultMaxAge, DefaultMaxSamples)
	fill(b, 0, 3)

	b.DiscardOldest(1)
	b.DiscardOldest(10)
	if b.Len() != 0 || b.Dropped() != 3 {
		t.Errorf("quedaron %d muestras y se contaron %d descartadas", b.Len(), b.Dropped())
	}
}

// El muestreo, el envío y la lectura del canal usan el buffer a la vez. Con
// -race, este test es el que lo verifica.
func TestConcurrentUse(t *testing.T) {
	b := NewBuffer(DefaultMaxAge, 50)
	var wg sync.WaitGroup

	wg.Go(func() { fill(b, 0, 2000) })
	wg.Go(func() {
		for range 2000 {
			batch := b.Next(20, t0)
			if len(batch.Samples) == 0 {
				continue
			}
			b.Sent(batch, len(batch.Samples), uuid.New(), t0)
			b.Ack(batch.Samples[len(batch.Samples)-1].TS)
		}
	})
	wg.Go(func() {
		for range 2000 {
			_ = b.Len()
			b.Restart()
		}
	})
	wg.Wait()

	if b.Len() > 50 {
		t.Errorf("el buffer pasó su tope: %d muestras", b.Len())
	}
}
