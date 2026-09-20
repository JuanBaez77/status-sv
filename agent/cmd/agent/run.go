package main

import (
	"context"
	"flag"

	"github.com/JuanBaez77/status-sv/agent/internal/config"
	"github.com/JuanBaez77/status-sv/agent/internal/state"
	"github.com/JuanBaez77/status-sv/agent/internal/ws"
	"github.com/JuanBaez77/status-sv/internal/buildinfo"
)

// runAgent mantiene el canal con el control plane hasta que llega la señal de
// apagado. Es lo que ejecuta la unidad de systemd.
func runAgent(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	stateFile := flags.String("state-file", "", "archivo de estado (o STATUSSV_STATE_FILE)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *stateFile != "" {
		cfg.StateFile = *stateFile
	}

	saved, err := state.Load(cfg.StateFile)
	if err != nil {
		return err
	}

	log := newLogger(cfg)
	log.InfoContext(ctx, "arrancando el agent",
		"version", buildinfo.Version,
		"commit", buildinfo.ShortCommit(),
		"servidor", saved.ServerName,
		"control_plane", saved.ServerURL,
	)

	return ws.NewClient(saved, buildinfo.Version, log).Run(ctx)
}
