//go:build integration

package metrics

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JuanBaez77/status-sv/proto"
)

func count(t *testing.T, pool *pgxpool.Pool, table string, serverID uuid.UUID) int {
	t.Helper()
	var n int
	// table viene de una lista fija de este test.
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table+" WHERE server_id = $1", serverID).Scan(&n); err != nil {
		t.Fatalf("contar %s: %v", table, err)
	}
	return n
}

func ingesterAt(pool *pgxpool.Pool, at time.Time) *Ingester {
	i := NewIngester(pool)
	i.now = func() time.Time { return at }
	return i
}

func TestIngestStoresEveryTableAndIsIdempotent(t *testing.T) {
	pool, q := setupDB(t)
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	if _, err := partitionsAt(q, at).Maintain(t.Context()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	server := createServer(t, q)
	batch := []proto.Sample{sampleAt(at.Add(-20 * time.Second)), sampleAt(at.Add(-10 * time.Second))}
	ingester := ingesterAt(pool, at)

	result, err := ingester.Ingest(t.Context(), server, batch)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if result.Accepted != 2 || result.New != 2 || !result.LastTS.Equal(at.Add(-10*time.Second)) {
		t.Errorf("resultado = %+v", result)
	}
	for table, want := range map[string]int{"metrics_raw": 2, "metrics_raw_disks": 4, "metrics_raw_networks": 2} {
		if got := count(t, pool, table, server); got != want {
			t.Errorf("%s tiene %d filas, se esperaban %d", table, got, want)
		}
	}

	// El mismo lote otra vez, como cuando se pierde un acuse: se acepta, no
	// se duplica nada y el agent puede soltarlo.
	result, err = ingester.Ingest(t.Context(), server, batch)
	if err != nil {
		t.Fatalf("reenvío: %v", err)
	}
	if result.Accepted != 2 || result.New != 0 {
		t.Errorf("reenvío: %+v, se esperaban 2 aceptadas y 0 nuevas", result)
	}
	if got := count(t, pool, "metrics_raw_disks", server); got != 4 {
		t.Errorf("el reenvío duplicó discos: %d filas", got)
	}
}

// Una muestra nunca queda guardada a medias: si falla la tabla de discos, la
// muestra tampoco se guarda y el agent la vuelve a mandar entera.
func TestIngestIsAllOrNothing(t *testing.T) {
	pool, q := setupDB(t)
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	if _, err := partitionsAt(q, at).Maintain(t.Context()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	server := createServer(t, q)

	// Sin la partición de discos de ese día, esa inserción falla después de
	// que la de muestras ya pasó.
	if _, err := pool.Exec(t.Context(), "DROP TABLE metrics_raw_disks_p20260921"); err != nil {
		t.Fatalf("borrar la partición: %v", err)
	}

	if _, err := ingesterAt(pool, at).Ingest(t.Context(), server, []proto.Sample{sampleAt(at)}); err == nil {
		t.Fatal("se esperaba un error al guardar los discos")
	}
	if got := count(t, pool, "metrics_raw", server); got != 0 {
		t.Errorf("quedaron %d muestras guardadas a medias", got)
	}
}

// Un lote de un servidor que ya no existe (se borró mientras el agent seguía
// conectado) no deja nada.
func TestIngestForADeletedServerFails(t *testing.T) {
	pool, q := setupDB(t)
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	if _, err := partitionsAt(q, at).Maintain(t.Context()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}

	missing := uuid.Must(uuid.NewV7())
	if _, err := ingesterAt(pool, at).Ingest(t.Context(), missing, []proto.Sample{sampleAt(at)}); err == nil {
		t.Fatal("se guardó un lote de un servidor que no existe")
	}
}
