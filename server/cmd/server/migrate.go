package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // driver database/sql que usa goose

	"github.com/JuanBaez77/status-sv/server/internal/config"
	"github.com/JuanBaez77/status-sv/server/migrations"
)

// runMigrate aplica o revierte migraciones. Nunca corre solo al arrancar el
// servidor: se ejecuta a propósito, para poder revisar antes de tocar
// producción (ver ADR-004).
func runMigrate(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("falta la acción: up, down o status")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("abrir la base: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("conectar con la base %s: %w", cfg.RedactedDatabaseURL(), err)
	}

	switch args[0] {
	case "up":
		applied, err := migrations.Up(ctx, db)
		if err != nil {
			return err
		}
		if applied == 0 {
			log.InfoContext(ctx, "no había migraciones pendientes")
			return nil
		}
		log.InfoContext(ctx, "migraciones aplicadas", "cantidad", applied)
		return nil

	case "down":
		if err := migrations.Down(ctx, db); err != nil {
			return err
		}
		log.InfoContext(ctx, "última migración revertida")
		return nil

	case "status":
		status, err := migrations.Status(ctx, db)
		if err != nil {
			return err
		}
		for _, migration := range status {
			applied := "pendiente"
			if migration.State == "applied" {
				applied = migration.AppliedAt.Format("2006-01-02 15:04:05")
			}
			fmt.Printf("%-50s %s\n", migration.Source.Path, applied)
		}
		return nil

	default:
		return fmt.Errorf("acción desconocida: %q; se espera up, down o status", args[0])
	}
}
