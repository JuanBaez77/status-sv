package collector

import (
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T, parts ...string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(append([]string{"testdata"}, parts...)...))
	if err != nil {
		t.Fatalf("leer la muestra: %v", err)
	}
	return string(raw)
}

func TestParseStat(t *testing.T) {
	tests := []struct {
		fixture   string
		wantTimes cpuTimes
		wantCores int
	}{
		// busy = user + nice + system + irq + softirq + steal; idle = idle + iowait.
		{fixture: "vps-ubuntu", wantTimes: cpuTimes{busy: 4705 + 356 + 5845 + 0 + 482 + 207, idle: 1623987 + 1541}, wantCores: 2},
		{fixture: "docker-desktop", wantTimes: cpuTimes{busy: 69800 + 361 + 78022 + 0 + 11564 + 0, idle: 16622430 + 21021}, wantCores: 6},
	}

	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			times, cores, err := parseStat(readFixture(t, tt.fixture, "stat"))
			if err != nil {
				t.Fatalf("parseStat: %v", err)
			}
			if times != tt.wantTimes {
				t.Errorf("contadores = %+v, se esperaba %+v", times, tt.wantTimes)
			}
			if cores != tt.wantCores {
				t.Errorf("núcleos = %d, se esperaban %d", cores, tt.wantCores)
			}
		})
	}
}

// Un kernel viejo trae solo user, nice, system e idle: alcanza para calcular.
func TestParseStatWithOldKernel(t *testing.T) {
	times, cores, err := parseStat("cpu  100 20 50 1000\ncpu0 100 20 50 1000\n")
	if err != nil {
		t.Fatalf("parseStat: %v", err)
	}
	if want := (cpuTimes{busy: 170, idle: 1000}); times != want {
		t.Errorf("contadores = %+v, se esperaba %+v", times, want)
	}
	if cores != 1 {
		t.Errorf("núcleos = %d, se esperaba 1", cores)
	}
}

func TestParseStatRejectsBrokenInput(t *testing.T) {
	tests := map[string]string{
		"sin línea cpu":        "cpu0 1 2 3 4\nctxt 5\n",
		"sin idle":             "cpu  1 2 3\n",
		"contador no numérico": "cpu  1 2 tres 4\n",
		"contador negativo":    "cpu  1 -2 3 4\n",
		"suma que desborda":    "cpu  18446744073709551615 1 0 0\n",
	}

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseStat(raw); err == nil {
				t.Fatal("se esperaba un error")
			}
		})
	}
}

// guest y guest_nice ya están dentro de user y nice: si se sumaran otra vez,
// una máquina con máquinas virtuales adentro parecería más ocupada.
func TestGuestTimeIsNotCountedTwice(t *testing.T) {
	prev, err := parseCPULine(" 1000 0 500 8000 0 0 0 0 300 0")
	if err != nil {
		t.Fatalf("primera lectura: %v", err)
	}
	// En 2000 ticks: 250 ocupados, de los cuales 100 fueron de una máquina
	// virtual (y por eso están en user y en guest a la vez).
	cur, err := parseCPULine(" 1200 0 550 9750 0 0 0 0 400 0")
	if err != nil {
		t.Fatalf("segunda lectura: %v", err)
	}

	got, ok := usedPercent(prev, cur)
	if !ok {
		t.Fatal("los contadores avanzaron y no se calculó el uso")
	}
	if got != 12.5 {
		t.Errorf("uso = %v%%, se esperaba 12.5%%", got)
	}
}

func TestUsedPercent(t *testing.T) {
	base := cpuTimes{busy: 10_000, idle: 90_000}

	tests := []struct {
		name   string
		cur    cpuTimes
		want   float64
		wantOK bool
	}{
		{name: "un octavo ocupado", cur: cpuTimes{busy: 10_250, idle: 91_750}, want: 12.5, wantOK: true},
		{name: "en reposo", cur: cpuTimes{busy: 10_000, idle: 92_000}, want: 0, wantOK: true},
		{name: "al máximo", cur: cpuTimes{busy: 12_000, idle: 90_000}, want: 100, wantOK: true},
		{name: "redondea a dos decimales", cur: cpuTimes{busy: 10_001, idle: 90_002}, want: 33.33, wantOK: true},
		{name: "los contadores no avanzaron", cur: base, wantOK: false},
		{name: "los contadores retrocedieron", cur: cpuTimes{busy: 9_000, idle: 80_000}, wantOK: false},
		// iowait puede bajar: lo ocioso retrocede aunque el total avance.
		{name: "lo ocioso retrocede", cur: cpuTimes{busy: 11_000, idle: 89_990}, want: 100, wantOK: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := usedPercent(base, tt.cur)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, se esperaba %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("uso = %v%%, se esperaba %v%%", got, tt.want)
			}
		})
	}
}
