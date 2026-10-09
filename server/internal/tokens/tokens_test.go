package tokens

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestParseScopes(t *testing.T) {
	got, err := ParseScopes([]string{"logs:production", "read:staging", "read:staging"})
	if err != nil {
		t.Fatalf("ParseScopes: %v", err)
	}
	want := []Scope{"read:staging", "logs:production"}
	if !slices.Equal(got, want) {
		t.Errorf("ParseScopes = %v, se esperaba %v (sin repetidos y en el orden de la lista)", got, want)
	}

	for _, raw := range [][]string{
		nil,
		{},
		{"read"},
		{"read:*"},
		{"deploy:staging"},
		{"READ:staging"},
		{"read:staging", "read:dev"},
	} {
		if _, err := ParseScopes(raw); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseScopes(%q) = %v, se esperaba ErrInvalid", raw, err)
		}
	}
}

func TestScopeParts(t *testing.T) {
	s := Scope("logs:production")
	if s.Capability() != CapLogs || s.Environment() != EnvProduction {
		t.Errorf("partes de %q = %q y %q", s, s.Capability(), s.Environment())
	}
	for _, known := range KnownScopes() {
		if known.Capability() == "" || known.Environment() == "" {
			t.Errorf("scope mal formado en la lista: %q", known)
		}
	}
}

func TestTokenAllows(t *testing.T) {
	listed := uuid.Must(uuid.NewV7())
	other := uuid.Must(uuid.NewV7())

	all := Token{Scopes: []Scope{"read:staging", "logs:staging"}}
	if !all.AllowsProject(CapRead, EnvStaging, other) || !all.AllowsProject(CapLogs, EnvStaging, other) {
		t.Error("un token sin lista tiene que alcanzar a cualquier proyecto de su entorno")
	}
	if all.AllowsProject(CapRead, EnvProduction, other) {
		t.Error("read:staging no puede leer producción")
	}
	if !all.CanRead() {
		t.Error("un token con read tiene que poder leer servidores")
	}

	restricted := Token{Scopes: []Scope{"read:production"}, ProjectIDs: []uuid.UUID{listed}}
	if !restricted.AllowsProject(CapRead, EnvProduction, listed) {
		t.Error("el proyecto de la lista tiene que estar alcanzado")
	}
	if restricted.AllowsProject(CapRead, EnvProduction, other) {
		t.Error("un proyecto fuera de la lista no puede estar alcanzado")
	}
	if restricted.AllowsProject(CapLogs, EnvProduction, listed) {
		t.Error("sin logs:production no hay logs, aunque el proyecto esté en la lista")
	}

	logsOnly := Token{Scopes: []Scope{"logs:staging"}}
	if logsOnly.CanRead() {
		t.Error("un token con solo logs no ve servidores")
	}
}

func TestTokenStatus(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	tok := Token{ExpiresAt: now.Add(time.Hour)}
	if got := tok.Status(now); got != StatusActive {
		t.Errorf("Status = %q, se esperaba active", got)
	}
	if got := tok.Status(now.Add(time.Hour)); got != StatusExpired {
		t.Errorf("Status al vencer = %q, se esperaba expired", got)
	}
	revoked := now.Add(-time.Minute)
	tok.RevokedAt = &revoked
	if got := tok.Status(now); got != StatusRevoked {
		t.Errorf("Status revocado = %q, se esperaba revoked", got)
	}
}

func TestNormalizeName(t *testing.T) {
	got, err := normalizeName("  claude-code juan  ")
	if err != nil || got != "claude-code juan" {
		t.Errorf("normalizeName = %q, %v", got, err)
	}
	if got, err := normalizeName(strings.Repeat("ñ", MaxNameLen)); err != nil || got == "" {
		t.Errorf("un nombre de %d caracteres con tildes tiene que valer: %v", MaxNameLen, err)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("a", MaxNameLen+1), "uno\ndos", "tab\there", "\x00"} {
		if _, err := normalizeName(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("normalizeName(%q) = %v, se esperaba ErrInvalid", bad, err)
		}
	}
}
