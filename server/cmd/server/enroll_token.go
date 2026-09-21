package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/JuanBaez77/status-sv/server/internal/config"
	"github.com/JuanBaez77/status-sv/server/internal/enrollment"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// runEnrollToken crea el token de un solo uso con el que se enrola un agent.
//
// El token se imprime una única vez: la base guarda nada más que su hash. Si se
// pierde, se genera otro. Va por stdout solo, para poder canalizarlo sin que se
// mezcle con los mensajes del programa.
func runEnrollToken(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("enroll-token", flag.ContinueOnError)
	serverName := flags.String("server", "", "nombre del servidor a enrolar (obligatorio)")
	createdBy := flags.String("by", "cli", "quién pide el token, para la auditoría")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if *serverName == "" {
		flags.Usage()
		return errors.New("falta --server")
	}
	if !enrollment.ValidServerName(*serverName) {
		return fmt.Errorf("%w: %q", enrollment.ErrInvalidServerName, *serverName)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)

	pool, err := store.OpenPool(ctx, store.Config{DSN: cfg.DatabaseURL, MaxConns: 2})
	if err != nil {
		return err
	}
	defer pool.Close()

	service := enrollment.NewService(pool, cfg.EnrollmentTTL, log)

	secret, token, err := service.CreateToken(ctx, *serverName, *createdBy)
	if err != nil {
		return err
	}

	fmt.Printf(`Token de enrolamiento para %q:

  %s

Vence el %s (en %s) y sirve una sola vez.
No vuelve a mostrarse: si se pierde, generá otro.

En el VPS, como root, con el binario del agent ya copiado:

  STATUSSV_SERVER_URL=https://<control-plane> ./install-agent.sh ./statussv-agent

y pegá el token cuando lo pida. No lo pases como argumento: quedaría visible en ps.

`,
		token.ServerName,
		secret.Reveal(),
		token.ExpiresAt.Local().Format(time.RFC1123),
		time.Until(token.ExpiresAt).Round(time.Second),
	)

	return nil
}
