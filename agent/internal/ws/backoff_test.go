package ws

import (
	"testing"
	"time"
)

// newTestBackoff usa un azar fijo para que las esperas sean comparables.
func newTestBackoff(random float64) *Backoff {
	return &Backoff{
		Base:   time.Second,
		Max:    time.Minute,
		random: func() float64 { return random },
	}
}

func TestBackoffGrowsAndStopsAtMax(t *testing.T) {
	// Con random = 1 la espera es el tope exponencial completo.
	backoff := newTestBackoff(1)

	want := []time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
		time.Minute, // 64s se recorta al tope
		time.Minute,
		time.Minute,
	}

	for i, expected := range want {
		if got := backoff.Next(); got != expected {
			t.Errorf("reintento %d: espera %v, se esperaba %v", i+1, got, expected)
		}
	}
}

// Con jitter, dos agents que se caen juntos no vuelven en el mismo instante.
func TestBackoffJitterStaysInRange(t *testing.T) {
	backoff := NewBackoff()

	for range 12 {
		delay := backoff.Next()
		if delay <= 0 {
			t.Fatalf("una espera no puede ser cero o negativa: %v", delay)
		}
		if delay > DefaultMaxDelay {
			t.Fatalf("la espera %v superó el tope %v", delay, DefaultMaxDelay)
		}
	}
}

func TestBackoffJitterHalvesTheCeiling(t *testing.T) {
	// Con random = 0 la espera es la mitad del tope exponencial: es el piso.
	backoff := newTestBackoff(0)

	if got := backoff.Next(); got != 500*time.Millisecond {
		t.Errorf("primera espera %v, se esperaban 500ms", got)
	}
	if got := backoff.Next(); got != time.Second {
		t.Errorf("segunda espera %v, se esperaba 1s", got)
	}
}

func TestBackoffResetStartsOver(t *testing.T) {
	backoff := newTestBackoff(1)

	for range 5 {
		backoff.Next()
	}
	if backoff.Attempts() != 5 {
		t.Errorf("Attempts() = %d, se esperaban 5", backoff.Attempts())
	}

	backoff.Reset()

	if backoff.Attempts() != 0 {
		t.Errorf("después de Reset, Attempts() = %d", backoff.Attempts())
	}
	if got := backoff.Next(); got != time.Second {
		t.Errorf("después de Reset la espera es %v, se esperaba 1s", got)
	}
}

// Una corrida muy larga sin reconectar no puede desbordar el cálculo.
func TestBackoffSurvivesManyAttempts(t *testing.T) {
	backoff := newTestBackoff(1)

	for range 200 {
		delay := backoff.Next()
		if delay <= 0 || delay > DefaultMaxDelay {
			t.Fatalf("con %d intentos la espera quedó en %v", backoff.Attempts(), delay)
		}
	}
}

func TestBackoffUsesDefaultsWhenUnset(t *testing.T) {
	var backoff Backoff // sin Base ni Max

	delay := backoff.Next()
	if delay <= 0 || delay > DefaultMaxDelay {
		t.Errorf("con valores por defecto la espera fue %v", delay)
	}
}
