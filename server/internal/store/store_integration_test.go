//go:build integration

// Tests contra un PostgreSQL de verdad: ejercitan las consultas generadas por
// sqlc sobre el esquema real. Se saltean si no hay TEST_DATABASE_URL.
//
// Cada test arranca con las tablas vacías, así que los de este paquete no se
// pueden correr en paralelo entre sí. Con los de otros paquetes no hay problema:
// testdb le da a cada paquete su propia base.
package store_test

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JuanBaez77/status-sv/server/internal/store"
	"github.com/JuanBaez77/status-sv/server/internal/testdb"
)

// uniqueViolation es el código de PostgreSQL para una violación de unicidad.
const uniqueViolation = "23505"

// setup deja las tablas vacías y devuelve las consultas listas para usar.
func setup(t *testing.T) (*store.Queries, *pgxpool.Pool) {
	t.Helper()

	pool := testdb.Setup(t)
	testdb.Truncate(t, pool)

	return store.New(pool), pool
}

func hashOf(seed string) []byte {
	sum := sha256.Sum256([]byte(seed))
	return sum[:]
}

func newID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("generar UUID v7: %v", err)
	}
	return id
}

func createServer(t *testing.T, q *store.Queries, name string) store.Server {
	t.Helper()
	server, err := q.CreateServer(t.Context(), store.CreateServerParams{
		ID:           newID(t),
		Name:         name,
		Hostname:     name + ".example.com",
		Labels:       []byte(`{"entorno":"pruebas"}`),
		AgentVersion: "0.0.1",
	})
	if err != nil {
		t.Fatalf("crear el servidor %q: %v", name, err)
	}
	return server
}

// isUniqueViolation dice si el error es un choque con un índice único.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}

// El esquema que dejan las migraciones es el que esperan las consultas: si
// faltara una tabla, todo lo demás fallaría con errores confusos.
func TestSchemaIsInPlace(t *testing.T) {
	_, pool := setup(t)

	for _, table := range []string{"servers", "enrollment_tokens", "agent_credentials", "goose_db_version"} {
		var exists bool
		err := pool.QueryRow(t.Context(),
			"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)",
			table).Scan(&exists)
		if err != nil {
			t.Fatalf("consultar por la tabla %s: %v", table, err)
		}
		if !exists {
			t.Errorf("falta la tabla %s", table)
		}
	}
}
func TestServerLifecycle(t *testing.T) {
	queries, _ := setup(t)
	ctx := t.Context()

	created := createServer(t, queries, "vps-01")
	if created.LastSeenAt != nil {
		t.Errorf("un servidor recién creado no debería tener last_seen_at: %v", created.LastSeenAt)
	}

	byID, err := queries.GetServerByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("buscar el servidor por ID: %v", err)
	}
	if byID.Name != "vps-01" {
		t.Errorf("GetServerByID devolvió el nombre %q", byID.Name)
	}

	// El nombre no distingue mayúsculas: VPS-01 es el mismo servidor.
	byName, err := queries.GetServerByName(ctx, "VPS-01")
	if err != nil {
		t.Fatalf("buscar el servidor por nombre en mayúsculas: %v", err)
	}
	if byName.ID != created.ID {
		t.Errorf("GetServerByName devolvió otro servidor: %v", byName.ID)
	}

	updated, err := queries.UpdateServerHostname(ctx, store.UpdateServerHostnameParams{
		ID:       created.ID,
		Hostname: "vps01.nuevo.example.com",
	})
	if err != nil {
		t.Fatalf("actualizar el hostname: %v", err)
	}
	if updated.Hostname != "vps01.nuevo.example.com" {
		t.Errorf("el hostname quedó en %q", updated.Hostname)
	}

	createServer(t, queries, "vps-02")
	servers, err := queries.ListServers(ctx)
	if err != nil {
		t.Fatalf("listar servidores: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("se esperaban 2 servidores, hay %d", len(servers))
	}
	if servers[0].Name != "vps-01" || servers[1].Name != "vps-02" {
		t.Errorf("la lista no vino ordenada por nombre: %q, %q", servers[0].Name, servers[1].Name)
	}

	deleted, err := queries.DeleteServer(ctx, created.ID)
	if err != nil {
		t.Fatalf("borrar el servidor: %v", err)
	}
	if deleted != 1 {
		t.Errorf("DeleteServer borró %d filas", deleted)
	}
	if _, err := queries.GetServerByID(ctx, created.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("el servidor borrado se sigue encontrando: %v", err)
	}
}

func TestServerNameIsUniqueIgnoringCase(t *testing.T) {
	queries, _ := setup(t)

	createServer(t, queries, "vps-01")

	_, err := queries.CreateServer(t.Context(), store.CreateServerParams{
		ID:       newID(t),
		Name:     "VPS-01",
		Hostname: "otro.example.com",
		Labels:   []byte(`{}`),
	})
	if !isUniqueViolation(err) {
		t.Fatalf("se esperaba una violación de unicidad por el nombre, se obtuvo: %v", err)
	}
}

func TestTouchServerUpdatesLastSeenAndVersion(t *testing.T) {
	queries, _ := setup(t)
	ctx := t.Context()

	server := createServer(t, queries, "vps-01")

	newVersion := "0.2.0"
	touched, err := queries.TouchServer(ctx, store.TouchServerParams{
		ID:           server.ID,
		AgentVersion: &newVersion,
	})
	if err != nil {
		t.Fatalf("registrar el latido: %v", err)
	}
	if touched.LastSeenAt == nil {
		t.Fatal("last_seen_at quedó vacío después del latido")
	}
	if touched.AgentVersion != newVersion {
		t.Errorf("la versión del agent quedó en %q", touched.AgentVersion)
	}

	// Un latido sin versión no borra la que ya estaba.
	withoutVersion, err := queries.TouchServer(ctx, store.TouchServerParams{ID: server.ID})
	if err != nil {
		t.Fatalf("registrar el latido sin versión: %v", err)
	}
	if withoutVersion.AgentVersion != newVersion {
		t.Errorf("un latido sin versión pisó la versión guardada: %q", withoutVersion.AgentVersion)
	}
	if !withoutVersion.LastSeenAt.After(*touched.LastSeenAt) && !withoutVersion.LastSeenAt.Equal(*touched.LastSeenAt) {
		t.Errorf("last_seen_at retrocedió: %v -> %v", touched.LastSeenAt, withoutVersion.LastSeenAt)
	}
}

func TestEnrollmentTokenCanOnlyBeUsedOnce(t *testing.T) {
	queries, _ := setup(t)
	ctx := t.Context()

	tokenHash := hashOf("token-de-enrolamiento")
	token, err := queries.CreateEnrollmentToken(ctx, store.CreateEnrollmentTokenParams{
		ID:         newID(t),
		TokenHash:  tokenHash,
		ServerName: "vps-01",
		ExpiresAt:  time.Now().Add(15 * time.Minute),
		CreatedBy:  "cli",
	})
	if err != nil {
		t.Fatalf("crear el token de enrolamiento: %v", err)
	}
	if token.UsedAt != nil {
		t.Error("un token recién creado no debería estar usado")
	}

	server := createServer(t, queries, "vps-01")

	consumed, err := queries.ConsumeEnrollmentToken(ctx, store.ConsumeEnrollmentTokenParams{
		TokenHash: tokenHash,
		ServerID:  &server.ID,
	})
	if err != nil {
		t.Fatalf("consumir el token: %v", err)
	}
	if consumed.UsedAt == nil || consumed.ServerID == nil || *consumed.ServerID != server.ID {
		t.Fatalf("el token consumido quedó inconsistente: usado=%v servidor=%v", consumed.UsedAt, consumed.ServerID)
	}

	// El segundo intento no devuelve ninguna fila: el token ya no sirve.
	_, err = queries.ConsumeEnrollmentToken(ctx, store.ConsumeEnrollmentTokenParams{
		TokenHash: tokenHash,
		ServerID:  &server.ID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("el token se pudo usar dos veces: %v", err)
	}
}

func TestExpiredEnrollmentTokenIsRejectedAndSwept(t *testing.T) {
	queries, _ := setup(t)
	ctx := t.Context()

	expiredHash := hashOf("token-vencido")
	if _, err := queries.CreateEnrollmentToken(ctx, store.CreateEnrollmentTokenParams{
		ID:         newID(t),
		TokenHash:  expiredHash,
		ServerName: "vps-01",
		ExpiresAt:  time.Now().Add(-time.Minute),
		CreatedBy:  "cli",
	}); err != nil {
		t.Fatalf("crear el token vencido: %v", err)
	}

	validHash := hashOf("token-vigente")
	if _, err := queries.CreateEnrollmentToken(ctx, store.CreateEnrollmentTokenParams{
		ID:         newID(t),
		TokenHash:  validHash,
		ServerName: "vps-02",
		ExpiresAt:  time.Now().Add(15 * time.Minute),
		CreatedBy:  "cli",
	}); err != nil {
		t.Fatalf("crear el token vigente: %v", err)
	}

	server := createServer(t, queries, "vps-01")
	_, err := queries.ConsumeEnrollmentToken(ctx, store.ConsumeEnrollmentTokenParams{
		TokenHash: expiredHash,
		ServerID:  &server.ID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("se consumió un token vencido: %v", err)
	}

	pending, err := queries.ListPendingEnrollmentTokens(ctx)
	if err != nil {
		t.Fatalf("listar tokens pendientes: %v", err)
	}
	if len(pending) != 1 || pending[0].ServerName != "vps-02" {
		t.Fatalf("los tokens pendientes no son los esperados: %+v", pending)
	}

	swept, err := queries.DeleteExpiredEnrollmentTokens(ctx)
	if err != nil {
		t.Fatalf("barrer los tokens vencidos: %v", err)
	}
	if swept != 1 {
		t.Errorf("el barrido borró %d tokens, se esperaba 1", swept)
	}
}

func TestOnlyOneActiveCredentialPerServer(t *testing.T) {
	queries, _ := setup(t)
	ctx := t.Context()

	server := createServer(t, queries, "vps-01")
	firstHash := hashOf("credencial-1")

	first, err := queries.CreateAgentCredential(ctx, store.CreateAgentCredentialParams{
		ID:         newID(t),
		ServerID:   server.ID,
		SecretHash: firstHash,
	})
	if err != nil {
		t.Fatalf("crear la primera credencial: %v", err)
	}

	// Mientras la primera siga activa, el índice parcial impide una segunda.
	_, err = queries.CreateAgentCredential(ctx, store.CreateAgentCredentialParams{
		ID:         newID(t),
		ServerID:   server.ID,
		SecretHash: hashOf("credencial-2"),
	})
	if !isUniqueViolation(err) {
		t.Fatalf("se esperaba una violación de unicidad por la credencial activa, se obtuvo: %v", err)
	}

	found, err := queries.GetActiveCredentialByHash(ctx, firstHash)
	if err != nil {
		t.Fatalf("buscar la credencial activa: %v", err)
	}
	if found.ID != first.ID {
		t.Errorf("se encontró otra credencial: %v", found.ID)
	}

	reason := "rotación de prueba"
	revoked, err := queries.RevokeAgentCredential(ctx, store.RevokeAgentCredentialParams{
		ID:     first.ID,
		Reason: &reason,
	})
	if err != nil {
		t.Fatalf("revocar la credencial: %v", err)
	}
	if revoked.RevokedAt == nil {
		t.Fatal("la credencial revocada quedó sin fecha de revocación")
	}

	// Una credencial revocada ya no autentica.
	if _, err := queries.GetActiveCredentialByHash(ctx, firstHash); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("la credencial revocada sigue autenticando: %v", err)
	}

	// Y ahora sí entra la nueva.
	if _, err := queries.CreateAgentCredential(ctx, store.CreateAgentCredentialParams{
		ID:         newID(t),
		ServerID:   server.ID,
		SecretHash: hashOf("credencial-2"),
	}); err != nil {
		t.Fatalf("crear la credencial de reemplazo: %v", err)
	}

	all, err := queries.ListCredentialsForServer(ctx, server.ID)
	if err != nil {
		t.Fatalf("listar las credenciales del servidor: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("el servidor debería tener 2 credenciales (una revocada), tiene %d", len(all))
	}
}

func TestDeletingServerCascadesToCredentials(t *testing.T) {
	queries, pool := setup(t)
	ctx := t.Context()

	server := createServer(t, queries, "vps-01")
	if _, err := queries.CreateAgentCredential(ctx, store.CreateAgentCredentialParams{
		ID:         newID(t),
		ServerID:   server.ID,
		SecretHash: hashOf("credencial"),
	}); err != nil {
		t.Fatalf("crear la credencial: %v", err)
	}

	if _, err := queries.DeleteServer(ctx, server.ID); err != nil {
		t.Fatalf("borrar el servidor: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_credentials").Scan(&count); err != nil {
		t.Fatalf("contar credenciales: %v", err)
	}
	if count != 0 {
		t.Errorf("quedaron %d credenciales huérfanas", count)
	}
}
