//go:build integration

package enrollment_test

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/enrollment"
	"github.com/JuanBaez77/status-sv/server/internal/testdb"
)

const testTTL = 15 * time.Minute

func setup(t *testing.T) (*enrollment.Service, *pgxpool.Pool) {
	t.Helper()

	pool := testdb.Setup(t)
	testdb.Truncate(t, pool)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return enrollment.NewService(pool, testTTL, log), pool
}
func TestCreateTokenStoresOnlyTheHash(t *testing.T) {
	service, pool := setup(t)
	ctx := t.Context()

	secret, token, err := service.CreateToken(ctx, "vps-01", "cli")
	if err != nil {
		t.Fatalf("crear el token: %v", err)
	}

	if secret.IsZero() {
		t.Fatal("el token en claro vino vacío")
	}
	if !secret.HasPrefix(auth.EnrollmentTokenPrefix) {
		t.Errorf("el token no tiene el prefijo esperado")
	}
	if token.ServerName != "vps-01" || token.CreatedBy != "cli" {
		t.Errorf("el token guardado no coincide: %+v", token)
	}
	if token.UsedAt != nil || token.ServerID != nil {
		t.Errorf("el token nació usado: %+v", token)
	}

	// Vence dentro de la ventana esperada, no antes ni mucho después.
	ttl := time.Until(token.ExpiresAt)
	if ttl <= 0 || ttl > testTTL {
		t.Errorf("el token vence en %v y el TTL configurado es %v", ttl, testTTL)
	}

	// Lo que quedó en la base es el hash, no el secreto.
	var stored []byte
	if err := pool.QueryRow(ctx, "SELECT token_hash FROM enrollment_tokens WHERE id = $1", token.ID).Scan(&stored); err != nil {
		t.Fatalf("leer el token guardado: %v", err)
	}
	if !auth.EqualHash(stored, secret.Hash()) {
		t.Error("el hash guardado no corresponde al token generado")
	}
	if strings.Contains(string(stored), secret.Reveal()) {
		t.Error("el secreto en claro terminó en la base")
	}
}

func TestCreateTokenRejectsBadServerNames(t *testing.T) {
	service, _ := setup(t)

	names := []string{
		"",
		" ",
		"con espacio",
		"-arranca-con-guion",
		"punto y coma; DROP TABLE servers",
		strings.Repeat("x", 65),
		"acentuado-ñ",
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			_, _, err := service.CreateToken(t.Context(), name, "cli")
			if !errors.Is(err, enrollment.ErrInvalidServerName) {
				t.Fatalf("se aceptó el nombre %q (error: %v)", name, err)
			}
		})
	}
}

func TestCreateTokenNeedsAnAuthor(t *testing.T) {
	service, _ := setup(t)

	if _, _, err := service.CreateToken(t.Context(), "vps-01", ""); err == nil {
		t.Fatal("se creó un token sin registrar quién lo pidió")
	}
}

func TestTwoTokensForTheSameServerAreDifferent(t *testing.T) {
	service, _ := setup(t)
	ctx := t.Context()

	first, _, err := service.CreateToken(ctx, "vps-01", "cli")
	if err != nil {
		t.Fatalf("crear el primer token: %v", err)
	}
	second, _, err := service.CreateToken(ctx, "vps-01", "cli")
	if err != nil {
		t.Fatalf("crear el segundo token: %v", err)
	}

	if auth.EqualHash(first.Hash(), second.Hash()) {
		t.Error("dos tokens para el mismo servidor salieron iguales")
	}
}

func TestSweepExpiredRemovesOnlyExpiredTokens(t *testing.T) {
	service, pool := setup(t)
	ctx := t.Context()

	if _, _, err := service.CreateToken(ctx, "vps-vigente", "cli"); err != nil {
		t.Fatalf("crear el token vigente: %v", err)
	}
	expired, _, err := service.CreateToken(ctx, "vps-vencido", "cli")
	if err != nil {
		t.Fatalf("crear el token a vencer: %v", err)
	}

	// Se lo envejece a mano, que es más rápido y determinista que esperar.
	if _, err := pool.Exec(ctx,
		"UPDATE enrollment_tokens SET expires_at = now() - interval '1 minute' WHERE token_hash = $1",
		expired.Hash()); err != nil {
		t.Fatalf("envejecer el token: %v", err)
	}

	deleted, err := service.SweepExpired(ctx)
	if err != nil {
		t.Fatalf("barrer los vencidos: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("se borraron %d tokens, se esperaba 1", deleted)
	}

	var left int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM enrollment_tokens").Scan(&left); err != nil {
		t.Fatalf("contar tokens: %v", err)
	}
	if left != 1 {
		t.Errorf("quedaron %d tokens, se esperaba 1", left)
	}
}
