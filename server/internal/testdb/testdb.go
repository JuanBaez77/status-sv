//go:build integration

// Package testdb le da a cada paquete de tests de integración su propia base de
// datos.
//
// Existe porque los tests de distintos paquetes corren en paralelo: si
// compartieran base, el TRUNCATE de uno borraría los datos del otro y las fallas
// serían intermitentes, que es la peor clase de falla.
package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // driver database/sql para goose

	"github.com/JuanBaez77/status-sv/server/internal/store"
	"github.com/JuanBaez77/status-sv/server/migrations"
)

// setupTimeout limita el armado y el desarmado de la base.
const setupTimeout = 30 * time.Second

// Setup crea una base propia, le aplica las migraciones y devuelve el pool.
// La base se borra sola cuando el paquete de tests termina.
//
// Si no hay TEST_DATABASE_URL, saltea los tests en vez de fallar: así el mismo
// comando de test sirve con o sin PostgreSQL a mano.
func Setup(t *testing.T) *pgxpool.Pool {
	t.Helper()

	baseDSN := envOrSkip(t)

	// CREATE y DROP DATABASE se hacen conectado a "postgres", la base de
	// mantenimiento que existe siempre. Así la base que nombra TEST_DATABASE_URL
	// no tiene que existir: su nombre se usa solo como prefijo.
	adminDSN, err := maintenanceDSN(baseDSN)
	if err != nil {
		t.Fatalf("armar la conexión de mantenimiento: %v", err)
	}

	name, dsn, err := uniqueDatabaseName(baseDSN)
	if err != nil {
		t.Fatalf("armar el nombre de la base de prueba: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), setupTimeout)
	defer cancel()

	if err := createDatabase(ctx, adminDSN, name); err != nil {
		t.Fatalf("crear la base de prueba: %v", err)
	}
	t.Cleanup(func() { dropDatabase(t, adminDSN, name) })

	migrateDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("abrir la base de prueba: %v", err)
	}
	if _, err := migrations.Up(ctx, migrateDB); err != nil {
		_ = migrateDB.Close()
		t.Fatalf("aplicar las migraciones: %v", err)
	}
	if err := migrateDB.Close(); err != nil {
		t.Fatalf("cerrar la conexión de migración: %v", err)
	}

	pool, err := store.OpenPool(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("abrir el pool: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

// Truncate deja las tablas vacías. Se llama al empezar cada test, porque los
// tests de un mismo paquete sí comparten la base.
func Truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	if _, err := pool.Exec(t.Context(),
		"TRUNCATE agent_credentials, enrollment_tokens, servers, users CASCADE"); err != nil {
		t.Fatalf("vaciar las tablas: %v", err)
	}
}

func envOrSkip(t *testing.T) string {
	t.Helper()

	dsn := strings.TrimSpace(envTestDatabaseURL())
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL no está definida: se saltean los tests de integración")
	}
	return dsn
}

// uniqueDatabaseName devuelve el nombre de la base nueva y su cadena de conexión.
func uniqueDatabaseName(adminDSN string) (string, string, error) {
	parsed, err := url.Parse(adminDSN)
	if err != nil {
		return "", "", fmt.Errorf("leer TEST_DATABASE_URL: %w", err)
	}

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return "", "", fmt.Errorf("generar el sufijo: %w", err)
	}

	base := strings.TrimPrefix(parsed.Path, "/")
	if base == "" {
		base = "statussv_test"
	}
	// PostgreSQL corta los nombres en 63 bytes, así que se acota la base.
	if len(base) > 40 {
		base = base[:40]
	}
	name := fmt.Sprintf("%s_%s", base, hex.EncodeToString(suffix))

	parsed.Path = "/" + name
	return name, parsed.String(), nil
}

func createDatabase(ctx context.Context, adminDSN, name string) error {
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		return fmt.Errorf("conectar para crear la base: %w", err)
	}
	defer func() { _ = admin.Close() }()

	// El nombre lo arma este paquete, pero se sanitiza igual: es un identificador
	// que va concatenado en la sentencia, y CREATE DATABASE no acepta parámetros.
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+quoted); err != nil {
		return fmt.Errorf("crear la base %s: %w", name, err)
	}
	return nil
}

func dropDatabase(t *testing.T, adminDSN, name string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), setupTimeout)
	defer cancel()

	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Logf("no se pudo conectar para borrar la base %s: %v", name, err)
		return
	}
	defer func() { _ = admin.Close() }()

	// FORCE cierra las conexiones que hayan quedado colgadas de un test que falló.
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)"); err != nil {
		t.Logf("no se pudo borrar la base de prueba %s: %v", name, err)
	}
}

// maintenanceDSN devuelve la misma conexión pero apuntada a la base "postgres".
func maintenanceDSN(dsn string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("leer TEST_DATABASE_URL: %w", err)
	}
	parsed.Path = "/postgres"
	return parsed.String(), nil
}
