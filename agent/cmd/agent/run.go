package main

import (
	"context"
	"flag"
	"sync"

	"github.com/JuanBaez77/status-sv/agent/internal/collector"
	"github.com/JuanBaez77/status-sv/agent/internal/config"
	"github.com/JuanBaez77/status-sv/agent/internal/metrics"
	"github.com/JuanBaez77/status-sv/agent/internal/state"
	"github.com/JuanBaez77/status-sv/agent/internal/ws"
	"github.com/JuanBaez77/status-sv/internal/buildinfo"
)

// runAgent mantiene el canal con el control plane hasta que llega la señal de
// apagado. Es lo que ejecuta la unidad de systemd.
//
// El muestreo corre aparte del canal: si la conexión se corta, las muestras se
// siguen tomando y esperan en el buffer a que vuelva.
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

	buffer := metrics.NewBuffer(metrics.DefaultMaxAge, metrics.DefaultMaxSamples)
	client := ws.NewClient(saved, buildinfo.Version, log, buffer)
	sampler := metrics.NewSampler(collector.New(), buffer, client.MetricsInterval, log)

	var wg sync.WaitGroup
	wg.Go(func() { sampler.Run(ctx) })
	err = client.Run(ctx)
	wg.Wait()
	return err
}
