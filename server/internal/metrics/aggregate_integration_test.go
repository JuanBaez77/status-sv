//go:build integration

package metrics

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JuanBaez77/status-sv/proto"
)

// minute12 es el minuto sobre el que trabajan estos tests.
var minute12 = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

// netSample es una muestra con un solo disco y una sola interfaz, con los
// valores que le interesan a cada test.
func netSample(ts time.Time, cpu float64, memUsedGiB, diskUsedGiB uint64, rx, tx uint64) proto.Sample {
	s := sampleAt(ts)
	s.CPU.UsedPercent = cpu
	s.Memory.UsedBytes = memUsedGiB << 30
	s.Disks = []proto.DiskSample{{
		Mountpoint: "/", TotalBytes: 80 << 30, UsedBytes: diskUsedGiB << 30, AvailableBytes: (70 - diskUsedGiB) << 30,
	}}
	s.Networks = []proto.NetworkSample{{Interface: "eth0", RxBytes: rx, TxBytes: tx}}
	return s
}

type minuteRow struct {
	samples            int
	cpuAvg, cpuMax     float32
	memAvg, memMax     int64
	diskUsedMax        int64
	diskAvailMin       int64
	rxAvg, rxMax       float64
	txAvg, txMax       float64
	hasDisk, hasNetwrk bool
}

func readMinute(t *testing.T, pool *pgxpool.Pool, serverID uuid.UUID, bucket time.Time) (minuteRow, bool) {
	t.Helper()
	ctx := t.Context()

	var r minuteRow
	err := pool.QueryRow(ctx, `
		SELECT samples, cpu_used_avg, cpu_used_max, mem_used_avg_bytes, mem_used_max_bytes
		FROM metrics_1m WHERE server_id = $1 AND bucket = $2`, serverID, bucket).
		Scan(&r.samples, &r.cpuAvg, &r.cpuMax, &r.memAvg, &r.memMax)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false
	}
	if err != nil {
		t.Fatalf("leer metrics_1m: %v", err)
	}

	err = pool.QueryRow(ctx, `
		SELECT used_max_bytes, available_min_bytes
		FROM metrics_1m_disks WHERE server_id = $1 AND bucket = $2 AND mountpoint = '/'`, serverID, bucket).
		Scan(&r.diskUsedMax, &r.diskAvailMin)
	switch {
	case err == nil:
		r.hasDisk = true
	case !errors.Is(err, pgx.ErrNoRows):
		t.Fatalf("leer metrics_1m_disks: %v", err)
	}

	err = pool.QueryRow(ctx, `
		SELECT rx_avg_per_sec, rx_max_per_sec, tx_avg_per_sec, tx_max_per_sec
		FROM metrics_1m_networks WHERE server_id = $1 AND bucket = $2 AND interface = 'eth0'`, serverID, bucket).
		Scan(&r.rxAvg, &r.rxMax, &r.txAvg, &r.txMax)
	switch {
	case err == nil:
		r.hasNetwrk = true
	case !errors.Is(err, pgx.ErrNoRows):
		t.Fatalf("leer metrics_1m_networks: %v", err)
	}
	return r, true
}

func mustMinute(t *testing.T, pool *pgxpool.Pool, serverID uuid.UUID, bucket time.Time) minuteRow {
	t.Helper()
	r, ok := readMinute(t, pool, serverID, bucket)
	if !ok {
		t.Fatalf("no hay agregado del minuto %v", bucket)
	}
	return r
}

func near(got, want float64) bool {
	return math.Abs(got-want) < 1e-6*math.Max(1, math.Abs(want))
}

// aggregateSetup deja la base con particiones y un servidor, y devuelve la
// ingesta con el reloj unos minutos después de minute12.
func aggregateSetup(t *testing.T) (*pgxpool.Pool, uuid.UUID, *Ingester) {
	t.Helper()
	pool, q := setupDB(t)
	at := minute12.Add(30 * time.Minute)
	if _, err := partitionsAt(q, at).Maintain(t.Context()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	return pool, createServer(t, q), ingesterAt(pool, at)
}

func ingest(t *testing.T, i *Ingester, serverID uuid.UUID, samples ...proto.Sample) {
	t.Helper()
	if _, err := i.Ingest(t.Context(), serverID, samples); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
}

func TestIngestAggregatesByMinute(t *testing.T) {
	pool, server, ingester := aggregateSetup(t)

	// La primera muestra es del minuto anterior: solo sirve de punto de
	// partida para la tasa de red del primer par.
	ingest(t, ingester, server,
		netSample(minute12.Add(-10*time.Second), 90, 7, 30, 0, 0),
		netSample(minute12, 10, 1, 20, 1000, 100),
		netSample(minute12.Add(10*time.Second), 20, 2, 25, 3000, 200),
		netSample(minute12.Add(20*time.Second), 60, 3, 22, 4000, 300),
	)

	got := mustMinute(t, pool, server, minute12)
	if got.samples != 3 {
		t.Errorf("samples = %d, se esperaban 3", got.samples)
	}
	if got.cpuAvg != 30 || got.cpuMax != 60 {
		t.Errorf("CPU = promedio %v, máximo %v; se esperaba 30 y 60", got.cpuAvg, got.cpuMax)
	}
	if got.memAvg != 2<<30 || got.memMax != 3<<30 {
		t.Errorf("memoria = promedio %d, máximo %d; se esperaba 2 y 3 GiB", got.memAvg, got.memMax)
	}
	// El peor momento del disco: lo más usado y lo menos disponible.
	if !got.hasDisk || got.diskUsedMax != 25<<30 || got.diskAvailMin != 45<<30 {
		t.Errorf("disco = usado %d, disponible %d; se esperaba 25 y 45 GiB", got.diskUsedMax, got.diskAvailMin)
	}
	// Tres pares de 10 s: 1000, 2000 y 1000 bytes recibidos.
	if !got.hasNetwrk || !near(got.rxAvg, 4000.0/30) || !near(got.rxMax, 200) {
		t.Errorf("rx = promedio %v, pico %v; se esperaba %v y 200", got.rxAvg, got.rxMax, 4000.0/30)
	}
	if !near(got.txAvg, 10) || !near(got.txMax, 10) {
		t.Errorf("tx = promedio %v, pico %v; se esperaba 10 y 10", got.txAvg, got.txMax)
	}

	// El minuto anterior tiene su muestra, pero no una tasa: le falta el par.
	prev := mustMinute(t, pool, server, minute12.Add(-time.Minute))
	if prev.samples != 1 || prev.hasNetwrk {
		t.Errorf("minuto anterior = %+v, se esperaba 1 muestra y sin red", prev)
	}
}

// Una muestra que llega tarde, en otro lote, deja el minuto igual que si
// hubiera llegado a tiempo. Y cambia también la tasa del minuto siguiente,
// porque parte en dos el par que cruzaba el minuto.
func TestLateSampleRecomputesItsMinuteAndTheNextRate(t *testing.T) {
	pool, server, ingester := aggregateSetup(t)

	ingest(t, ingester, server,
		netSample(minute12, 10, 1, 20, 0, 0),
		netSample(minute12.Add(20*time.Second), 60, 3, 22, 2000, 0),
		// Par de 50 s que cruza el minuto: 3000 bytes, 60 por segundo.
		netSample(minute12.Add(70*time.Second), 10, 1, 20, 5000, 0),
	)
	before := mustMinute(t, pool, server, minute12.Add(time.Minute))
	if !near(before.rxMax, 60) {
		t.Fatalf("pico del minuto siguiente = %v, se esperaba 60", before.rxMax)
	}

	// Llegan tarde dos muestras del primer minuto. La de :50 parte el par que
	// cruzaba: de :50 a 1:10 son 2800 bytes en 20 s, 140 por segundo.
	ingest(t, ingester, server,
		netSample(minute12.Add(10*time.Second), 20, 2, 25, 1000, 0),
		netSample(minute12.Add(50*time.Second), 20, 2, 25, 2200, 0),
	)

	got := mustMinute(t, pool, server, minute12)
	if got.samples != 4 || got.cpuMax != 60 || got.diskUsedMax != 25<<30 {
		t.Errorf("minuto recalculado = %+v, se esperaban 4 muestras, CPU máx 60 y disco 25 GiB", got)
	}
	// Pares del minuto: 1000/10, 1000/10 y 200/30.
	if !near(got.rxAvg, 2200.0/50) || !near(got.rxMax, 100) {
		t.Errorf("rx = promedio %v, pico %v; se esperaba %v y 100", got.rxAvg, got.rxMax, 2200.0/50)
	}
	after := mustMinute(t, pool, server, minute12.Add(time.Minute))
	if !near(after.rxMax, 140) || !near(after.rxAvg, 140) {
		t.Errorf("minuto siguiente = promedio %v, pico %v; se esperaba 140", after.rxAvg, after.rxMax)
	}

	// Reenviar un lote no cambia nada.
	ingest(t, ingester, server, netSample(minute12.Add(10*time.Second), 20, 2, 25, 1000, 0))
	if again := mustMinute(t, pool, server, minute12); again != got {
		t.Errorf("el reenvío cambió el minuto: %+v, antes %+v", again, got)
	}
}

// Un contador que baja es un reinicio del servidor, y un par separado por más
// de MaxRateGap es un agent que estuvo caído: ninguno de los dos es una tasa.
func TestNetworkRatesSkipCounterResetsAndGaps(t *testing.T) {
	pool, server, ingester := aggregateSetup(t)

	ingest(t, ingester, server,
		netSample(minute12, 10, 1, 20, 50_000, 0),
		netSample(minute12.Add(10*time.Second), 10, 1, 20, 100, 0), // reinicio
		netSample(minute12.Add(20*time.Second), 10, 1, 20, 1100, 0),
	)
	got := mustMinute(t, pool, server, minute12)
	if !near(got.rxAvg, 100) || !near(got.rxMax, 100) {
		t.Errorf("rx = promedio %v, pico %v; se esperaba 100: el reinicio no es una tasa", got.rxAvg, got.rxMax)
	}

	later := minute12.Add(MaxRateGap + time.Minute)
	ingest(t, ingester, server, netSample(later, 10, 1, 20, 900_000, 0))
	gap := mustMinute(t, pool, server, later.Truncate(time.Minute))
	if gap.hasNetwrk {
		t.Errorf("hay tasa después de %v sin muestras: %+v", MaxRateGap, gap)
	}
}
