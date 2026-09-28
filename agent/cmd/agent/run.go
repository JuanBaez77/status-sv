package main

import (
	"context"
	"flag"
	goruntime "runtime"
	"sync"

	"github.com/JuanBaez77/status-sv/agent/internal/collector"
	"github.com/JuanBaez77/status-sv/agent/internal/commands"
	"github.com/JuanBaez77/status-sv/agent/internal/config"
	"github.com/JuanBaez77/status-sv/agent/internal/journal"
	"github.com/JuanBaez77/status-sv/agent/internal/metrics"
	"github.com/JuanBaez77/status-sv/agent/internal/runtimestate"
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

	// El estado de runtime lee /proc, cgroups, D-Bus y el socket de Docker:
	// fuera de Linux no hay nada de eso, y el agent solo se usa para desarrollar.
	if goruntime.GOOS == "linux" {
		outbox := runtimestate.NewOutbox()
		client.SetRuntime(outbox)
		parts := newRuntimeParts(cfg, log)
		// Los comandos solo operan sobre lo que el agent vio en su última foto.
		client.SetCommands(commands.NewExecutor(parts.dockerClient, journal.NewReader(), parts.assembler.Knows, log))
		runner := runtimestate.NewRunner(parts.assembler, outbox, log, parts.dockerSource, parts.systemdSource)
		wg.Go(func() { runner.Run(ctx) })
	}

	err = client.Run(ctx)
	wg.Wait()
	return err
}
