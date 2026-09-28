package usage

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func TestFirstReadingHasNoPercent(t *testing.T) {
	tr := NewTracker(4)
	if _, ok := tr.CPUPercent("a", time.Second, t0); ok {
		t.Fatal("la primera lectura no tiene contra qué comparar")
	}
}

func TestPercentIsNormalizedToCores(t *testing.T) {
	tests := []struct {
		name  string
		cores int
		cpu   time.Duration // tiempo de CPU consumido en 10 s
		want  float64
	}{
		{name: "un núcleo entero de cuatro", cores: 4, cpu: 10 * time.Second, want: 25},
		{name: "medio núcleo de dos", cores: 2, cpu: 5 * time.Second, want: 25},
		{name: "todos los núcleos", cores: 4, cpu: 40 * time.Second, want: 100},
		{name: "ocioso", cores: 4, cpu: 0, want: 0},
		{name: "núcleos sin dato cuenta como uno", cores: 0, cpu: time.Second, want: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := NewTracker(tt.cores)
			tr.CPUPercent("a", time.Minute, t0)
			got, ok := tr.CPUPercent("a", time.Minute+tt.cpu, t0.Add(10*time.Second))
			if !ok {
				t.Fatal("la segunda lectura tendría que dar porcentaje")
			}
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("obtuve %v, esperaba %v", got, tt.want)
			}
		})
	}
}

// Los contadores y el reloj no se leen en el mismo instante exacto: un pico de
// redondeo no puede pasar de 100 y hacer rechazar el mensaje.
func TestPercentIsCappedAt100(t *testing.T) {
	tr := NewTracker(1)
	tr.CPUPercent("a", 0, t0)
	got, _ := tr.CPUPercent("a", 11*time.Second, t0.Add(10*time.Second))
	if got != 100 {
		t.Errorf("obtuve %v, esperaba 100", got)
	}
}

func TestCounterResetStartsOver(t *testing.T) {
	tr := NewTracker(1)
	tr.CPUPercent("a", time.Hour, t0)
	if _, ok := tr.CPUPercent("a", time.Second, t0.Add(10*time.Second)); ok {
		t.Fatal("un contador que volvió atrás no puede dar porcentaje")
	}
	got, ok := tr.CPUPercent("a", 6*time.Second, t0.Add(20*time.Second))
	if !ok || math.Abs(got-50) > 1e-9 {
		t.Errorf("después del reinicio esperaba 50%%, obtuve %v (%v)", got, ok)
	}
}

func TestClockGoingBackwardsHasNoPercent(t *testing.T) {
	tr := NewTracker(1)
	tr.CPUPercent("a", 0, t0)
	if _, ok := tr.CPUPercent("a", time.Second, t0); ok {
		t.Fatal("sin tiempo transcurrido no hay porcentaje")
	}
}

func TestUnitsAreIndependent(t *testing.T) {
	tr := NewTracker(1)
	tr.CPUPercent("a", 0, t0)
	tr.CPUPercent("b", 0, t0)
	a, _ := tr.CPUPercent("a", time.Second, t0.Add(10*time.Second))
	b, _ := tr.CPUPercent("b", 5*time.Second, t0.Add(10*time.Second))
	if math.Abs(a-10) > 1e-9 || math.Abs(b-50) > 1e-9 {
		t.Errorf("obtuve a=%v y b=%v, esperaba 10 y 50", a, b)
	}
}

func TestSweepForgetsUnitsNotSeen(t *testing.T) {
	tr := NewTracker(1)
	tr.CPUPercent("sigue", 0, t0)
	tr.CPUPercent("se-borra", 0, t0)
	tr.Sweep()
	if tr.Len() != 2 {
		t.Fatalf("después del primer ciclo esperaba 2 unidades, hay %d", tr.Len())
	}

	tr.CPUPercent("sigue", time.Second, t0.Add(10*time.Second))
	tr.Sweep()
	if tr.Len() != 1 {
		t.Fatalf("la unidad que no se leyó tendría que olvidarse; quedan %d", tr.Len())
	}
	if _, ok := tr.CPUPercent("se-borra", time.Second, t0.Add(20*time.Second)); ok {
		t.Error("una unidad olvidada vuelve a empezar sin porcentaje")
	}
}

func TestPercentHasTwoDecimals(t *testing.T) {
	tr := NewTracker(3)
	tr.CPUPercent("a", 0, t0)
	got, _ := tr.CPUPercent("a", time.Second, t0.Add(10*time.Second))
	if got != 3.33 {
		t.Errorf("obtuve %v, esperaba 3.33", got)
	}
}
