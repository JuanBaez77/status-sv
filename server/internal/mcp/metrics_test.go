package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/server/internal/tokens"
)

var metricsStart = time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)

// rawInfo arma tres horas de muestras cada 10 s, con la CPU subiendo.
func rawInfo() MetricsInfo {
	info := MetricsInfo{Resolution: "raw", From: metricsStart, To: metricsStart.Add(3 * time.Hour)}
	for i := range 1080 {
		cpu := float64(i) / 1079 * 50 // de 0 a 50 %
		info.Points = append(info.Points, MetricPoint{
			At: metricsStart.Add(time.Duration(i) * 10 * time.Second), CPU: cpu, CPUMax: cpu,
			MemUsed: 2 << 30, MemUsedMax: 2 << 30, MemTotal: 8 << 30, Load1: 0.5,
		})
	}
	info.Disks = []DiskSeries{{Mountpoint: "/", Points: []DiskPoint{
		{At: metricsStart, UsedBytes: 80, TotalBytes: 100},
		{At: metricsStart.Add(time.Hour), UsedBytes: 90, TotalBytes: 100},
		{At: metricsStart.Add(2 * time.Hour), UsedBytes: 85, TotalBytes: 100},
	}}}
	info.Networks = []NetworkSeries{{Interface: "eth0", Points: []NetworkPoint{
		{At: metricsStart, Rx: 100, RxMax: 100, Tx: 10, TxMax: 10},
		{At: metricsStart.Add(time.Minute), Rx: 300, RxMax: 900, Tx: 30, TxMax: 30},
	}}}
	return info
}

func TestSummarize(t *testing.T) {
	out := summarize("sv-gcg", "6h", rawInfo())

	if out.Samples != 1080 || out.Resolution != "raw" || out.From != "2026-10-09T09:00:00Z" || out.To != "2026-10-09T12:00:00Z" {
		t.Errorf("encabezado = %+v", out)
	}
	if out.CPU == nil || out.CPU.Last != 50 || out.CPU.Avg != 25 || out.CPU.Max != 50 {
		t.Errorf("cpu = %+v", out.CPU)
	}
	if out.Memory == nil || out.Memory.Avg != 25 || out.MemoryTotalBytes != 8<<30 {
		t.Errorf("memoria = %+v, total %d", out.Memory, out.MemoryTotalBytes)
	}
	if out.Swap != nil {
		t.Errorf("sin swap no va el campo, y quedó %+v", out.Swap)
	}
	if len(out.Disks) != 1 || out.Disks[0].LastPercent != 85 || out.Disks[0].MaxPercent != 90 {
		t.Errorf("discos = %+v", out.Disks)
	}
	if len(out.Networks) != 1 || out.Networks[0].RxAvgPerSecond != 200 || out.Networks[0].RxMaxPerSecond != 900 {
		t.Errorf("red = %+v", out.Networks)
	}

	series := out.Series
	if series == nil || len(series.Timestamps) != maxSeriesPoints || len(series.CPUPercent) != maxSeriesPoints {
		t.Fatalf("la serie tiene que tener %d puntos: %+v", maxSeriesPoints, series)
	}
	if series.Bucket != "3m0s" || series.Timestamps[0] != "2026-10-09T09:00:00Z" {
		t.Errorf("tramo %q, primero %q", series.Bucket, series.Timestamps[0])
	}
	if first, last := series.CPUPercent[0], series.CPUPercent[maxSeriesPoints-1]; first >= last {
		t.Errorf("la serie tiene que mostrar la subida: de %v a %v", first, last)
	}
}

func TestSummarizeWithoutSamples(t *testing.T) {
	out := summarize("sv-gcg", "1h", MetricsInfo{Resolution: "raw", From: metricsStart, To: metricsStart.Add(time.Hour)})
	if out.Samples != 0 || out.CPU != nil || out.Series != nil || out.Disks == nil || out.Networks == nil {
		t.Errorf("sin muestras = %+v", out)
	}
}

func TestDownsampleSkipsEmptyBuckets(t *testing.T) {
	info := MetricsInfo{From: metricsStart, To: metricsStart.Add(time.Hour), Points: []MetricPoint{
		{At: metricsStart, CPU: 10, CPUMax: 20, MemUsed: 1, MemTotal: 2},
		{At: metricsStart.Add(30 * time.Second), CPU: 30, CPUMax: 40, MemUsed: 1, MemTotal: 2},
		{At: metricsStart.Add(59 * time.Minute), CPU: 5, CPUMax: 5, MemUsed: 1, MemTotal: 4},
	}}
	got := downsample(info)
	if len(got.Timestamps) != 2 {
		t.Fatalf("dos tramos con datos, y quedaron %d: %+v", len(got.Timestamps), got)
	}
	if got.CPUPercent[0] != 20 || got.CPUMaxPercent[0] != 40 || got.MemoryPercent[0] != 50 {
		t.Errorf("primer tramo = %v %v %v", got.CPUPercent[0], got.CPUMaxPercent[0], got.MemoryPercent[0])
	}
	if got.Timestamps[1] != "2026-10-09T09:59:00Z" || got.MemoryPercent[1] != 25 {
		t.Errorf("último tramo = %v %v", got.Timestamps[1], got.MemoryPercent[1])
	}
}

func TestGetServerMetrics(t *testing.T) {
	f := newFixture(t)
	f.backend.metrics = rawInfo()
	f.addToken("lector", []tokens.Scope{"read:production"})
	session := f.connect(t, "lector")

	out, errText := call[serverMetricsOutput](t, session, "get_server_metrics", map[string]any{"server": "sv-gcg"})
	if errText != "" {
		t.Fatalf("get_server_metrics: %s", errText)
	}
	if out.Server != "sv-gcg" || out.Range != "1h" || out.CPU == nil {
		t.Errorf("salida = %+v", out)
	}
	if _, errText := call[serverMetricsOutput](t, session, "get_server_metrics", map[string]any{"server": serverB.String(), "range": "30d"}); errText != "" {
		t.Fatalf("por ID: %s", errText)
	}
	want := []metricsAsk{{server: serverA, span: time.Hour}, {server: serverB, span: 30 * 24 * time.Hour}}
	if len(f.backend.asked) != 2 || f.backend.asked[0] != want[0] || f.backend.asked[1] != want[1] {
		t.Errorf("pedidos al backend = %+v", f.backend.asked)
	}

	if _, errText := call[serverMetricsOutput](t, session, "get_server_metrics", map[string]any{"server": "sv-gcg", "range": "2h"}); !strings.Contains(errText, "range") {
		t.Errorf("un rango fuera de la lista lo rechaza el esquema: %q", errText)
	}

	entries := f.recorder.all()
	if len(entries) != 2 || entries[0].TargetType != "server" || entries[0].TargetID != serverA.String() || entries[0].Detail["rango"] != "1h" || entries[0].Detail["servidor"] != "sv-gcg" {
		t.Errorf("auditoría = %+v", entries)
	}
}

func TestGetServerMetricsOnlyForVisibleServers(t *testing.T) {
	f := newFixture(t)
	f.addToken("solo-tienda", []tokens.Scope{"read:production"}, projShop)
	session := f.connect(t, "solo-tienda")

	_, hidden := call[serverMetricsOutput](t, session, "get_server_metrics", map[string]any{"server": "cristales"})
	_, missing := call[serverMetricsOutput](t, session, "get_server_metrics", map[string]any{"server": "no-existe"})
	if !strings.Contains(hidden, "no hay un servidor") || strings.ReplaceAll(hidden, "cristales", "X") != strings.ReplaceAll(missing, "no-existe", "X") {
		t.Errorf("uno que no ve y uno que no existe tienen que dar el mismo error: %q y %q", hidden, missing)
	}
	if len(f.backend.asked) != 0 {
		t.Errorf("no se pidieron métricas de un servidor que no ve: %+v", f.backend.asked)
	}
	for _, e := range f.recorder.all() {
		if e.Result != resultNotFound {
			t.Errorf("resultado = %q, se esperaba not_found", e.Result)
		}
	}

	if _, errText := call[serverMetricsOutput](t, session, "get_server_metrics", map[string]any{"server": "sv-gcg"}); errText != "" {
		t.Errorf("el servidor de su proyecto sí: %s", errText)
	}
}
