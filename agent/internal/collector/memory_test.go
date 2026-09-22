package collector

import (
	"testing"

	"github.com/JuanBaez77/status-sv/proto"
)

const kib = 1024

func TestParseMeminfo(t *testing.T) {
	tests := []struct {
		fixture string
		want    proto.MemorySample
	}{
		{
			fixture: "vps-ubuntu",
			want: proto.MemorySample{
				TotalBytes:     8039548 * kib,
				UsedBytes:      (8039548 - 5123456) * kib,
				SwapTotalBytes: 2097148 * kib,
				SwapUsedBytes:  (2097148 - 1835004) * kib,
			},
		},
		{
			fixture: "docker-desktop",
			want: proto.MemorySample{
				TotalBytes:     8104672 * kib,
				UsedBytes:      (8104672 - 7233044) * kib,
				SwapTotalBytes: 2097152 * kib,
				SwapUsedBytes:  0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			got, err := parseMeminfo(readFixture(t, tt.fixture, "meminfo"))
			if err != nil {
				t.Fatalf("parseMeminfo: %v", err)
			}
			if got != tt.want {
				t.Errorf("memoria = %+v, se esperaba %+v", got, tt.want)
			}
		})
	}
}

// Los kernels anteriores a 3.14 no tienen MemAvailable.
func TestParseMeminfoWithoutMemAvailable(t *testing.T) {
	raw := "MemTotal: 1000 kB\nMemFree: 200 kB\nBuffers: 50 kB\nCached: 250 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n"

	got, err := parseMeminfo(raw)
	if err != nil {
		t.Fatalf("parseMeminfo: %v", err)
	}
	if want := uint64(1000-200-50-250) * kib; got.UsedBytes != want {
		t.Errorf("usada = %d, se esperaba %d", got.UsedBytes, want)
	}
}

// Un /proc virtualizado puede informar más disponible que total; la muestra
// tiene que seguir cumpliendo el contrato.
func TestParseMeminfoNeverReportsMoreUsedThanTotal(t *testing.T) {
	raw := "MemTotal: 1000 kB\nMemAvailable: 1500 kB\nSwapTotal: 100 kB\nSwapFree: 300 kB\n"

	got, err := parseMeminfo(raw)
	if err != nil {
		t.Fatalf("parseMeminfo: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("la memoria no cumple el contrato: %v", err)
	}
	if got.UsedBytes != 0 || got.SwapUsedBytes != 0 {
		t.Errorf("se esperaba uso cero, llegó %+v", got)
	}
}

func TestParseMeminfoRejectsBrokenInput(t *testing.T) {
	tests := map[string]string{
		"sin MemTotal":        "MemFree: 100 kB\n",
		"unidad desconocida":  "MemTotal: 100 MB\n",
		"valor no numérico":   "MemTotal: mucho kB\n",
		"valor que desborda":  "MemTotal: 18446744073709551615 kB\n",
		"campo roto que usa":  "MemTotal: 100 kB\nSwapFree: kB\n",
		"sin unidad":          "MemTotal: 100\n",
		"con campos de sobra": "MemTotal: 100 kB extra\n",
	}

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseMeminfo(raw); err == nil {
				t.Fatal("se esperaba un error")
			}
		})
	}
}

func TestParseLoadavg(t *testing.T) {
	got, err := parseLoadavg(readFixture(t, "vps-ubuntu", "loadavg"))
	if err != nil {
		t.Fatalf("parseLoadavg: %v", err)
	}
	if want := (proto.LoadSample{Load1: 0.42, Load5: 0.37, Load15: 0.31}); got != want {
		t.Errorf("carga = %+v, se esperaba %+v", got, want)
	}
}

func TestParseLoadavgRejectsBrokenInput(t *testing.T) {
	tests := map[string]string{
		"vacío":         "",
		"incompleto":    "0.1 0.2\n",
		"no numérico":   "0.1 mucha 0.3 1/2 3\n",
		"negativo":      "0.1 -0.2 0.3 1/2 3\n",
		"NaN":           "NaN 0.2 0.3 1/2 3\n",
		"infinito":      "0.1 +Inf 0.3 1/2 3\n",
		"con separador": "0,1 0,2 0,3 1/2 3\n",
	}

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseLoadavg(raw); err == nil {
				t.Fatal("se esperaba un error")
			}
		})
	}
}
