//go:build integration

// Tests contra un PostgreSQL de verdad: las particiones las crean y las borran
// funciones de la migración, y eso solo se puede probar con la base real.
package metrics

import (
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JuanBaez77/status-sv/server/internal/store"
	"github.com/JuanBaez77/status-sv/server/internal/testdb"
)

const (
	checkViolation  = "23514"
	uniqueViolation = "23505"
)

// setupDB devuelve una base con las migraciones aplicadas, sin servidores ni
// particiones de métricas.
func setupDB(t *testing.T) (*pgxpool.Pool, *store.Queries) {
	t.Helper()

	pool := testdb.Setup(t)
	testdb.Truncate(t, pool)
	q := store.New(pool)

	// Las particiones son DDL: el TRUNCATE no las borra, y los tests de este
	// paquete comparten la base.
	for _, pol := range policies() {
		for _, name := range partitionsOf(t, pool, pol.table) {
			if _, err := pool.Exec(t.Context(), "DROP TABLE "+pgxIdentifier(name)); err != nil {
				t.Fatalf("borrar la partición %s: %v", name, err)
			}
		}
	}
	return pool, q
}

// pgxIdentifier escapa un nombre de tabla que viene del catálogo, para este
// test y nada más.
func pgxIdentifier(name string) string {
	return `"` + name + `"`
}

func partitionsOf(t *testing.T, pool *pgxpool.Pool, table string) []string {
	t.Helper()

	rows, err := pool.Query(t.Context(), `
		SELECT c.relname
		FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = $1::regclass
		ORDER BY c.relname`, table)
	if err != nil {
		t.Fatalf("listar las particiones de %s: %v", table, err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("leer una partición: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("listar las particiones de %s: %v", table, err)
	}
	return names
}

func partitionsAt(q *store.Queries, at time.Time) *Partitions {
	p := NewPartitions(q, slog.New(slog.DiscardHandler))
	p.now = func() time.Time { return at }
	return p
}

func createServer(t *testing.T, q *store.Queries) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	if _, err := q.CreateServer(t.Context(), store.CreateServerParams{
		ID: id, Name: "vps-" + id.String()[:8], Hostname: "vps", Labels: []byte(`{}`), AgentVersion: "0.1.0",
	}); err != nil {
		t.Fatalf("crear el servidor: %v", err)
	}
	return id
}

func insertRaw(t *testing.T, pool *pgxpool.Pool, serverID uuid.UUID, ts time.Time) error {
	t.Helper()
	_, err := pool.Exec(t.Context(), `
		INSERT INTO metrics_raw (server_id, ts, cpu_used_percent, cpu_iowait_percent, cpu_steal_percent, cpu_cores,
			mem_total_bytes, mem_used_bytes, swap_total_bytes, swap_used_bytes, load1, load5, load15)
		VALUES ($1, $2, 12.5, 0.5, 1, 2, 8000, 3000, 2000, 0, 0.4, 0.3, 0.2)`, serverID, ts)
	return err
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestPartitionsLifecycle(t *testing.T) {
	pool, q := setupDB(t)
	monday := time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC)

	// Primera pasada: del 19 al 23 en el crudo (48 h para atrás y para
	// adelante) y de agosto a octubre en el agregado por minuto.
	result, err := partitionsAt(q, monday).Maintain(t.Context())
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if result != (Result{Created: 3*5 + 3*3, Dropped: 0}) {
		t.Errorf("primera pasada: %+v, se esperaban 24 creadas", result)
	}
	wantRaw := []string{"metrics_raw_p20260919", "metrics_raw_p20260920", "metrics_raw_p20260921", "metrics_raw_p20260922", "metrics_raw_p20260923"}
	if got := partitionsOf(t, pool, "metrics_raw"); !slices.Equal(got, wantRaw) {
		t.Errorf("particiones del crudo = %v, se esperaba %v", got, wantRaw)
	}
	wantMinute := []string{"metrics_1m_disks_p202608", "metrics_1m_disks_p202609", "metrics_1m_disks_p202610"}
	if got := partitionsOf(t, pool, "metrics_1m_disks"); !slices.Equal(got, wantMinute) {
		t.Errorf("particiones del agregado = %v, se esperaba %v", got, wantMinute)
	}

	// Una segunda pasada en el mismo momento no hace nada.
	if again, err := partitionsAt(q, monday).Maintain(t.Context()); err != nil || again != (Result{}) {
		t.Errorf("segunda pasada: %+v, %v; se esperaba que no hiciera nada", again, err)
	}

	// Tres días después: se crean el 24, 25 y 26, y se borran las que
	// terminaron antes del 22 a las 15:00 (19, 20 y 21).
	thursday := monday.AddDate(0, 0, 3)
	result, err = partitionsAt(q, thursday).Maintain(t.Context())
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if result != (Result{Created: 3 * 3, Dropped: 3 * 3}) {
		t.Errorf("tres días después: %+v, se esperaban 9 creadas y 9 borradas", result)
	}
	wantRaw = []string{"metrics_raw_p20260922", "metrics_raw_p20260923", "metrics_raw_p20260924", "metrics_raw_p20260925", "metrics_raw_p20260926"}
	if got := partitionsOf(t, pool, "metrics_raw"); !slices.Equal(got, wantRaw) {
		t.Errorf("particiones del crudo = %v, se esperaba %v", got, wantRaw)
	}
}

// Los cortes caen a medianoche UTC aunque la sesión esté en otra zona horaria.
func TestPartitionBoundariesAreUTC(t *testing.T) {
	pool, q := setupDB(t)
	if _, err := partitionsAt(q, time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)).Maintain(t.Context()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	serverID := createServer(t, q)

	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("tomar una conexión: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(t.Context(), "SET TimeZone = 'America/Argentina/Buenos_Aires'"); err != nil {
		t.Fatalf("cambiar la zona horaria: %v", err)
	}
	// Se devuelve la conexión al pool con la zona de siempre.
	defer func() { _, _ = conn.Exec(t.Context(), "RESET TimeZone") }()

	tests := []struct {
		ts        time.Time
		partition string
	}{
		{ts: time.Date(2026, 9, 22, 23, 59, 59, 999_000_000, time.UTC), partition: "metrics_raw_p20260922"},
		{ts: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC), partition: "metrics_raw_p20260923"},
	}
	for _, tt := range tests {
		if _, err := conn.Exec(t.Context(), `
			INSERT INTO metrics_raw (server_id, ts, cpu_used_percent, cpu_iowait_percent, cpu_steal_percent, cpu_cores,
				mem_total_bytes, mem_used_bytes, swap_total_bytes, swap_used_bytes, load1, load5, load15)
			VALUES ($1, $2, 1, 0, 0, 2, 100, 50, 0, 0, 0, 0, 0)`, serverID, tt.ts); err != nil {
			t.Fatalf("insertar la muestra de %v: %v", tt.ts, err)
		}

		var partition string
		if err := conn.QueryRow(t.Context(),
			"SELECT tableoid::regclass::text FROM metrics_raw WHERE server_id = $1 AND ts = $2", serverID, tt.ts,
		).Scan(&partition); err != nil {
			t.Fatalf("buscar la muestra: %v", err)
		}
		if partition != tt.partition {
			t.Errorf("la muestra de %v quedó en %s, se esperaba %s", tt.ts, partition, tt.partition)
		}
	}
}

// No hay partición por defecto: una muestra fuera de rango se rechaza, y la
// ingesta tiene que filtrarlas antes.
func TestSampleWithoutPartitionIsRejected(t *testing.T) {
	pool, q := setupDB(t)
	if _, err := partitionsAt(q, time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)).Maintain(t.Context()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	serverID := createServer(t, q)

	err := insertRaw(t, pool, serverID, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if pgCode(err) != checkViolation {
		t.Fatalf("una muestra sin partición devolvió %v, se esperaba el error %s", err, checkViolation)
	}
}

// La ingesta se apoya en la clave primaria para no duplicar un lote reenviado.
func TestRawSamplesAreUniquePerServerAndInstant(t *testing.T) {
	pool, q := setupDB(t)
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	if _, err := partitionsAt(q, at).Maintain(t.Context()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	serverID := createServer(t, q)

	if err := insertRaw(t, pool, serverID, at); err != nil {
		t.Fatalf("primera muestra: %v", err)
	}
	if err := insertRaw(t, pool, serverID, at); pgCode(err) != uniqueViolation {
		t.Fatalf("la muestra repetida devolvió %v, se esperaba el error %s", err, uniqueViolation)
	}
}

func TestConstraintsRejectImpossibleValues(t *testing.T) {
	pool, q := setupDB(t)
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	if _, err := partitionsAt(q, at).Maintain(t.Context()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	serverID := createServer(t, q)

	tests := map[string]string{
		"disco con más disponible que libre": `INSERT INTO metrics_raw_disks VALUES ($1, $2, '/', 100, 60, 41)`,
		"disco con nombre vacío":             `INSERT INTO metrics_raw_disks VALUES ($1, $2, '', 100, 60, 40)`,
		"contador de red negativo":           `INSERT INTO metrics_raw_networks VALUES ($1, $2, 'eth0', -1, 0)`,
		"memoria usada mayor que la total": `INSERT INTO metrics_raw (server_id, ts, cpu_used_percent, cpu_iowait_percent, cpu_steal_percent, cpu_cores,
			mem_total_bytes, mem_used_bytes, swap_total_bytes, swap_used_bytes, load1, load5, load15)
			VALUES ($1, $2, 1, 0, 0, 2, 100, 101, 0, 0, 0, 0, 0)`,
		"CPU fuera de rango": `INSERT INTO metrics_raw (server_id, ts, cpu_used_percent, cpu_iowait_percent, cpu_steal_percent, cpu_cores,
			mem_total_bytes, mem_used_bytes, swap_total_bytes, swap_used_bytes, load1, load5, load15)
			VALUES ($1, $2, 1, 0, 101, 2, 100, 50, 0, 0, 0, 0, 0)`,
	}
	for name, statement := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := pool.Exec(t.Context(), statement, serverID, at); pgCode(err) != checkViolation {
				t.Fatalf("devolvió %v, se esperaba el error %s", err, checkViolation)
			}
		})
	}

	if _, err := pool.Exec(t.Context(), `INSERT INTO metrics_raw_disks VALUES ($1, $2, '/', 100, 60, 35)`, serverID, at); err != nil {
		t.Errorf("un disco válido fue rechazado: %v", err)
	}
}

// Al borrar un servidor se van sus métricas, en todas las particiones.
func TestDeletingAServerDeletesItsMetrics(t *testing.T) {
	pool, q := setupDB(t)
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	if _, err := partitionsAt(q, at).Maintain(t.Context()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	serverID := createServer(t, q)
	for _, ts := range []time.Time{at, at.Add(24 * time.Hour)} {
		if err := insertRaw(t, pool, serverID, ts); err != nil {
			t.Fatalf("insertar: %v", err)
		}
	}

	if _, err := q.DeleteServer(t.Context(), serverID); err != nil {
		t.Fatalf("borrar el servidor: %v", err)
	}
	var left int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM metrics_raw WHERE server_id = $1", serverID).Scan(&left); err != nil {
		t.Fatalf("contar: %v", err)
	}
	if left != 0 {
		t.Errorf("quedaron %d muestras del servidor borrado", left)
	}
}

// Una partición creada a mano, con otro nombre, no se borra nunca sola.
func TestDropLeavesForeignPartitionsAlone(t *testing.T) {
	pool, q := setupDB(t)
	if _, err := pool.Exec(t.Context(),
		`CREATE TABLE metrics_raw_manual PARTITION OF metrics_raw FOR VALUES FROM ('2020-01-01Z') TO ('2020-01-02Z')`); err != nil {
		t.Fatalf("crear la partición a mano: %v", err)
	}
	if _, err := partitionsAt(q, time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)).Maintain(t.Context()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}

	if !slices.Contains(partitionsOf(t, pool, "metrics_raw"), "metrics_raw_manual") {
		t.Error("se borró una partición que no creó el mantenimiento")
	}
}

// Las funciones solo aceptan las tablas de métricas.
func TestPartitionFunctionsRejectOtherTables(t *testing.T) {
	_, q := setupDB(t)
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	if _, err := q.EnsureMetricPartitions(t.Context(), store.EnsureMetricPartitionsParams{Parent: "servers", FromTs: at, ToTs: at}); err == nil {
		t.Error("se aceptó crear particiones de servers")
	}
	if _, err := q.DropMetricPartitions(t.Context(), store.DropMetricPartitionsParams{Parent: "servers", BeforeTs: at}); err == nil {
		t.Error("se aceptó borrar particiones de servers")
	}
}
