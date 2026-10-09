package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/server/internal/audit"
	"github.com/JuanBaez77/status-sv/server/internal/config"
	"github.com/JuanBaez77/status-sv/server/internal/store"
	"github.com/JuanBaez77/status-sv/server/internal/tokens"
)

// runTokenRevoke revoca un token del MCP, o todos. Es el freno de emergencia
// para cuando el panel no está a mano (ADR-025): se corre por SSH en el
// servidor del control plane.
func runTokenRevoke(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("token-revoke", flag.ContinueOnError)
	id := flags.String("id", "", "ID del token a revocar")
	all := flags.Bool("all", false, "revocar todos los tokens que siguen sin revocar")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if (*id == "") == !*all {
		flags.Usage()
		return errors.New("hace falta --id o --all, y uno solo")
	}
	var tokenID uuid.UUID
	if *id != "" {
		parsed, err := uuid.Parse(*id)
		if err != nil {
			return fmt.Errorf("--id no es un UUID: %w", err)
		}
		tokenID = parsed
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pool, err := store.OpenPool(ctx, store.Config{DSN: cfg.DatabaseURL, MaxConns: 2})
	if err != nil {
		return err
	}
	defer pool.Close()
	service := tokens.NewService(pool, newLogger(cfg))
	recorder := audit.NewRecorder(store.New(pool))

	if *all {
		n, err := service.RevokeAll(ctx, audit.ActorCLI)
		if err != nil {
			return err
		}
		recordCLI(ctx, recorder, audit.ActionTokenRevoke, "api_token", "todos", map[string]any{"revocados": n})
		fmt.Fprintf(os.Stderr, "Tokens revocados: %d.\n", n)
		return nil
	}

	tok, err := service.Revoke(ctx, tokenID, audit.ActorCLI)
	if err != nil {
		return err
	}
	recordCLI(ctx, recorder, audit.ActionTokenRevoke, "api_token", tok.ID.String(),
		map[string]any{"prefijo": tok.Prefix, "nombre": tok.Name})
	fmt.Fprintf(os.Stderr, "Token %q (%s) revocado.\n", tok.Name, tok.Prefix)
	return nil
}
