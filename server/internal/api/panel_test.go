package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// testNow es el instante fijo con el que trabajan estos tests.
var testNow = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

func TestServerStatus(t *testing.T) {
	const offlineAfter = 45 * time.Second
	recent := testNow.Add(-10 * time.Second)
	old := testNow.Add(-time.Minute)

	tests := []struct {
		name     string
		lastSeen *time.Time
		want     string
	}{
		{"nunca se vio", nil, statusNeverSeen},
		{"latido reciente", &recent, statusOnline},
		{"silencio largo", &old, statusOffline},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serverStatus(tt.lastSeen, testNow, offlineAfter); got != tt.want {
				t.Errorf("serverStatus = %q, se esperaba %q", got, tt.want)
			}
		})
	}
}

func TestParseRange(t *testing.T) {
	tests := []struct {
		name           string
		query          string
		wantResolution string
		wantFrom       time.Time
		wantTo         time.Time
		wantErr        bool
	}{
		{
			name:  "sin parámetros: la última hora, en crudo",
			query: "", wantResolution: resolutionRaw,
			wantFrom: testNow.Add(-time.Hour), wantTo: testNow,
		},
		{
			name:  "rango corto: crudo",
			query: "from=2026-09-22T10:00:00Z&to=2026-09-22T12:00:00Z", wantResolution: resolutionRaw,
			wantFrom: time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC), wantTo: testNow,
		},
		{
			name:  "rango largo: por minuto",
			query: "from=2026-09-21T12:00:00Z&to=2026-09-22T12:00:00Z", wantResolution: resolutionMinute,
			wantFrom: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC), wantTo: testNow,
		},
		{
			name:  "resolución pedida a mano",
			query: "resolution=1m", wantResolution: resolutionMinute,
			wantFrom: testNow.Add(-time.Hour), wantTo: testNow,
		},
		{
			// El panel manda la hora local; se pasa a UTC.
			name:  "con otro huso horario",
			query: "from=2026-09-22T08:00:00-03:00&to=2026-09-22T09:00:00-03:00", wantResolution: resolutionRaw,
			wantFrom: time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC),
			wantTo:   time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
		},
		{name: "from después de to", query: "from=2026-09-22T12:00:00Z&to=2026-09-22T10:00:00Z", wantErr: true},
		{name: "rango vacío", query: "from=2026-09-22T12:00:00Z&to=2026-09-22T12:00:00Z", wantErr: true},
		{name: "rango enorme", query: "from=2020-01-01T00:00:00Z&to=2026-09-22T12:00:00Z", wantErr: true},
		{name: "fecha ilegible", query: "from=ayer", wantErr: true},
		{name: "resolución inventada", query: "resolution=1s", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/servers/x/metrics?"+tt.query, nil)
			got, err := parseRange(r, testNow)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("se esperaba un error, salió %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRange: %v", err)
			}
			if got.resolution != tt.wantResolution || !got.from.Equal(tt.wantFrom) || !got.to.Equal(tt.wantTo) {
				t.Errorf("parseRange = %v a %v en %q; se esperaba %v a %v en %q",
					got.from, got.to, got.resolution, tt.wantFrom, tt.wantTo, tt.wantResolution)
			}
		})
	}
}

func TestRatesFromCounters(t *testing.T) {
	at := func(seconds int) time.Time { return testNow.Add(time.Duration(seconds) * time.Second) }
	rows := []store.ListRawNetworksRow{
		// eth0: dos pares normales, después un reinicio del contador.
		{Ts: at(0), Interface: "eth0", RxBytes: 1000, TxBytes: 0},
		{Ts: at(10), Interface: "eth0", RxBytes: 2000, TxBytes: 100},
		{Ts: at(20), Interface: "eth0", RxBytes: 5000, TxBytes: 200},
		{Ts: at(30), Interface: "eth0", RxBytes: 100, TxBytes: 0},
		// eth1: un hueco largo, que no es una tasa.
		{Ts: at(0), Interface: "eth1", RxBytes: 0, TxBytes: 0},
		{Ts: at(3600), Interface: "eth1", RxBytes: 900_000, TxBytes: 0},
	}

	got := ratesFromCounters(rows)
	if len(got) != 1 || got[0].Interface != "eth0" {
		t.Fatalf("series = %+v; se esperaba solo eth0", got)
	}
	if len(got[0].Points) != 2 {
		t.Fatalf("%d puntos, se esperaban 2 (el reinicio no cuenta): %+v", len(got[0].Points), got[0].Points)
	}
	if got[0].Points[0].RxPerSec != 100 || got[0].Points[1].RxPerSec != 300 {
		t.Errorf("tasas = %v y %v; se esperaba 100 y 300", got[0].Points[0].RxPerSec, got[0].Points[1].RxPerSec)
	}
	if got[0].Points[0].TxPerSec != 10 {
		t.Errorf("tasa de salida = %v, se esperaba 10", got[0].Points[0].TxPerSec)
	}
	// Cada punto lleva la marca de la segunda muestra del par.
	if !got[0].Points[0].TS.Equal(at(10)) {
		t.Errorf("el punto quedó en %v, se esperaba %v", got[0].Points[0].TS, at(10))
	}
}

func TestPercent(t *testing.T) {
	if got := percent(1, 4); got != 25 {
		t.Errorf("percent(1, 4) = %v, se esperaba 25", got)
	}
	// Un disco de tamaño cero no existe, pero no puede dividir por cero.
	if got := percent(1, 0); got != 0 {
		t.Errorf("percent(1, 0) = %v, se esperaba 0", got)
	}
}
