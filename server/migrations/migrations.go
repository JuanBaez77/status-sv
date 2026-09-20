// Package migrations lleva embebidas las migraciones SQL del control plane y las
// aplica con goose. Ir embebidas quiere decir que desplegar es copiar un binario:
// no hay una carpeta de SQL que mantener sincronizada con el servidor.
//
// Las migraciones nunca se aplican solas al arrancar: siempre con un subcomando
// explícito, para poder revisarlas antes de tocar producción (ver ADR-004).
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
)

// FS lleva los archivos .sql de esta carpeta embebidos en el binario.
//
//go:embed *.sql
var FS embed.FS

func newProvider(db *sql.DB) (*goose.Provider, error) {
	provider, err := goose.NewProvider(goose.DialectPostgres, db, FS)
	if err != nil {
		return nil, fmt.Errorf("preparar migraciones: %w", err)
	}
	return provider, nil
}

// Up aplica todas las migraciones pendientes y devuelve cuántas aplicó.
func Up(ctx context.Context, db *sql.DB) (int, error) {
	provider, err := newProvider(db)
	if err != nil {
		return 0, err
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return 0, fmt.Errorf("aplicar migraciones: %w", err)
	}
	return len(results), nil
}

// Down revierte la última migración aplicada.
func Down(ctx context.Context, db *sql.DB) error {
	provider, err := newProvider(db)
	if err != nil {
		return err
	}
	if _, err := provider.Down(ctx); err != nil {
		return fmt.Errorf("revertir la última migración: %w", err)
	}
	return nil
}

// Status devuelve el estado de cada migración, aplicada o pendiente.
func Status(ctx context.Context, db *sql.DB) ([]*goose.MigrationStatus, error) {
	provider, err := newProvider(db)
	if err != nil {
		return nil, err
	}
	status, err := provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("consultar el estado de las migraciones: %w", err)
	}
	return status, nil
}
