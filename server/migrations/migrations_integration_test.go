//go:build integration

package migrations_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // driver database/sql para goose
	"github.com/pressly/goose/v3"

	"github.com/JuanBaez77/status-sv/server/internal/testdb"
	"github.com/JuanBaez77/status-sv/server/migrations"
)

// La migración 00006 llega a una base que ya tiene agregado por minuto: tiene
// que cargar la hora de esos datos, con sus propias particiones, y poder
// revertirse.
func TestHourlyMigrationBackfillsExistingMinutes(t *testing.T) {
	pool := testdb.Setup(t)
	ctx := t.Context()

	db, err := sql.Open("pgx", pool.Config().ConnString())
	if err != nil {
		t.Fatalf("abrir la base: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		t.Fatalf("preparar las migraciones: %v", err)
	}
	if _, err := provider.DownTo(ctx, 5); err != nil {
		t.Fatalf("volver a la migración 5: %v", err)
	}

	// Con SQL propio y no con store: las consultas generadas siguen el esquema
	// de la última migración, y acá la base está en la 5.
	serverID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO servers (id, name, hostname, labels, agent_version)
		VALUES ($1, 'vps-backfill', 'vps', '{}', '0.1.0')`, serverID); err != nil {
		t.Fatalf("crear el servidor: %v", err)
	}
	noon := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	for _, parent := range []string{"metrics_1m", "metrics_1m_disks", "metrics_1m_networks"} {
		if _, err := pool.Exec(ctx, "SELECT metrics_ensure_partitions($1, $2, $3)", parent, noon, noon.Add(time.Hour)); err != nil {
			t.Fatalf("crear las particiones de %s: %v", parent, err)
		}
	}
	// Dos minutos de la hora 12: CPU promedio 15 con dos muestras y 90 con una.
	for _, m := range []struct {
		minute  int
		samples int
		cpu     float32
		memGiB  int64
		diskGiB int64
		rx      float64
	}{{0, 2, 15, 2, 25, 100}, {1, 1, 90, 5, 22, 200}} {
		bucket := noon.Add(time.Duration(m.minute) * time.Minute)
		if _, err := pool.Exec(ctx, `
			INSERT INTO metrics_1m VALUES ($1, $2, $3, $4, $4, 0, 0, 0, 2, $5, $6, $6, 0, 0, 0, 0, 0, 0)`,
			serverID, bucket, m.samples, m.cpu, int64(8)<<30, m.memGiB<<30); err != nil {
			t.Fatalf("insertar el minuto %d: %v", m.minute, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO metrics_1m_disks VALUES ($1, $2, '/', $3, $4, $5)`,
			serverID, bucket, int64(80)<<30, m.diskGiB<<30, (80-m.diskGiB)<<30); err != nil {
			t.Fatalf("insertar el disco del minuto %d: %v", m.minute, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO metrics_1m_networks VALUES ($1, $2, 'eth0', $3, $3, 0, 0)`,
			serverID, bucket, m.rx); err != nil {
			t.Fatalf("insertar la red del minuto %d: %v", m.minute, err)
		}
	}

	if _, err := provider.UpTo(ctx, 6); err != nil {
		t.Fatalf("aplicar la migración 6: %v", err)
	}

	var (
		samples                int
		cpuMin, cpuAvg, cpuMax float32
		memMin, memAvg, memMax int64
		diskUsed               int64
		rxAvg, rxMax           float64
	)
	if err := pool.QueryRow(ctx, `
		SELECT h.samples, h.cpu_used_min, h.cpu_used_avg, h.cpu_used_max,
		       h.mem_used_min_bytes, h.mem_used_avg_bytes, h.mem_used_max_bytes,
		       d.used_max_bytes, n.rx_avg_per_sec, n.rx_max_per_sec
		FROM metrics_1h h
		JOIN metrics_1h_disks d USING (server_id, bucket)
		JOIN metrics_1h_networks n USING (server_id, bucket)
		WHERE h.server_id = $1 AND h.bucket = $2`, serverID, noon).
		Scan(&samples, &cpuMin, &cpuAvg, &cpuMax, &memMin, &memAvg, &memMax, &diskUsed, &rxAvg, &rxMax); err != nil {
		t.Fatalf("leer la hora cargada: %v", err)
	}
	if samples != 3 || cpuMin != 15 || cpuAvg != 40 || cpuMax != 90 {
		t.Errorf("CPU = %d muestras, %v / %v / %v; se esperaba 3 muestras, 15 / 40 / 90", samples, cpuMin, cpuAvg, cpuMax)
	}
	if memMin != 2<<30 || memAvg != 3<<30 || memMax != 5<<30 {
		t.Errorf("memoria = %d / %d / %d; se esperaba 2 / 3 / 5 GiB", memMin, memAvg, memMax)
	}
	if diskUsed != 25<<30 || rxAvg != 150 || rxMax != 200 {
		t.Errorf("disco = %d, rx = %v / %v; se esperaba 25 GiB y 150 / 200", diskUsed, rxAvg, rxMax)
	}

	// La carga creó la partición con el nombre del mantenimiento.
	var partition string
	if err := pool.QueryRow(ctx, "SELECT tableoid::regclass::text FROM metrics_1h WHERE server_id = $1", serverID).
		Scan(&partition); err != nil || partition != "metrics_1h_p202609" {
		t.Errorf("partición = %q (%v), se esperaba metrics_1h_p202609", partition, err)
	}

	// Revertir y volver a aplicar deja todo igual que antes.
	if _, err := provider.DownTo(ctx, 5); err != nil {
		t.Fatalf("revertir la migración 6: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("volver a aplicar las migraciones: %v", err)
	}
}
