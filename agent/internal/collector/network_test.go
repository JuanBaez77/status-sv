package collector

import (
	"slices"
	"testing"

	"github.com/JuanBaez77/status-sv/proto"
)

func TestParseNetDev(t *testing.T) {
	tests := []struct {
		fixture string
		want    []proto.NetworkSample
	}{
		{
			// Quedan afuera lo, docker0, la red br-* y los dos veth.
			fixture: "vps-ubuntu",
			want: []proto.NetworkSample{
				{Interface: "ens3", RxBytes: 98123456789, TxBytes: 12345678901},
				{Interface: "wg0", RxBytes: 5242880, TxBytes: 2621440},
			},
		},
		{
			fixture: "docker-desktop",
			want: []proto.NetworkSample{
				{Interface: "eth0", RxBytes: 360675968, TxBytes: 130559915},
				{Interface: "services1", RxBytes: 125026709, TxBytes: 15082668},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			got, err := parseNetDev(readFixture(t, tt.fixture, "net", "dev"))
			if err != nil {
				t.Fatalf("parseNetDev: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("interfaces = %+v\nse esperaba   %+v", got, tt.want)
			}
		})
	}
}

// Con contadores grandes, los kernels viejos pegan el número a los dos puntos.
func TestParseNetDevWithNumberGluedToColon(t *testing.T) {
	raw := "  eth0:4294967296 5 0 0 0 0 0 0 5678 6 0 0 0 0 0 0\n"

	got, err := parseNetDev(raw)
	if err != nil {
		t.Fatalf("parseNetDev: %v", err)
	}
	want := []proto.NetworkSample{{Interface: "eth0", RxBytes: 4294967296, TxBytes: 5678}}
	if !slices.Equal(got, want) {
		t.Errorf("interfaces = %+v, se esperaba %+v", got, want)
	}
}

func TestParseNetDevRejectsBrokenInput(t *testing.T) {
	tests := map[string]string{
		"pocos contadores":      "  eth0: 1 2 3 4\n",
		"recibidos no numérico": "  eth0: x 0 0 0 0 0 0 0 5 0 0 0 0 0 0 0\n",
		"enviados no numérico":  "  eth0: 1 0 0 0 0 0 0 0 y 0 0 0 0 0 0 0\n",
	}

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseNetDev(raw); err == nil {
				t.Fatal("se esperaba un error")
			}
		})
	}
}

func TestIgnoredInterface(t *testing.T) {
	tests := map[string]bool{
		"lo":              true,
		"veth94baec7":     true,
		"docker0":         true,
		"docker_gwbridge": true,
		"br-2b9f3ac87269": true,
		"eth0":            false,
		"ens3":            false,
		"enp1s0":          false,
		"wg0":             false,
		"bond0":           false,
		"loopback-real":   false,
	}

	for name, want := range tests {
		if got := ignoredInterface(name); got != want {
			t.Errorf("ignoredInterface(%q) = %v, se esperaba %v", name, got, want)
		}
	}
}
