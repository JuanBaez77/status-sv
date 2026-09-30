package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sync"

	"github.com/JuanBaez77/status-sv/agent/internal/collector"
	"github.com/JuanBaez77/status-sv/agent/internal/commands"
	"github.com/JuanBaez77/status-sv/agent/internal/config"
	"github.com/JuanBaez77/status-sv/agent/internal/deploy"
	"github.com/JuanBaez77/status-sv/agent/internal/journal"
	"github.com/JuanBaez77/status-sv/agent/internal/metrics"
	"github.com/JuanBaez77/status-sv/agent/internal/projects"
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
		executor := commands.NewExecutor(parts.dockerClient, journal.NewReader(), parts.assembler.Knows, log)
		client.SetCommands(executor)

		// El motor de deploy (ADR-015). Viene apagado: lo prende una persona
		// en el servidor. Apagado igual se arma, para contestar disabled y
		// para mandar lo que haya quedado de antes.
		deploys := deploy.NewOutbox()
		engine, err := newDeployEngine(ctx, cfg, parts, deploys, log)
		if err != nil {
			return err
		}
		executor.SetDeployer(engine)
		client.SetDeploys(deploys)
		wg.Go(engine.Recover)
		defer engine.Wait()
		runner := runtimestate.NewRunner(parts.assembler, outbox, log, parts.dockerSource, parts.systemdSource)
		wg.Go(func() { runner.Run(ctx) })

		// Los proyectos declarados en el servidor (ADR-012), en solo lectura,
		// con su healthcheck pasivo cada minuto.
		projectsOutbox := projects.NewOutbox()
		client.SetProjects(projectsOutbox)
		declared := projects.NewRunner(cfg.ProjectsDir, projectsOutbox, projects.NewChecker(buildinfo.Version), log)
		wg.Go(func() { declared.Run(ctx) })
	}

	err = client.Run(ctx)
	wg.Wait()
	return err
}

// newDeployEngine arma el motor de deploy con las piezas de verdad. git y
// docker se buscan una vez, al arrancar: si faltan, el motor contesta que no
// está disponible en vez de fallar a mitad de un deploy.
func newDeployEngine(ctx context.Context, cfg config.Config, parts runtimeParts, out *deploy.Outbox, log *slog.Logger) (*deploy.Engine, error) {
	engineCfg := deploy.Config{
		Enabled:        cfg.DeployEnabled,
		StateDir:       filepath.Dir(cfg.StateFile),
		ProjectsDir:    cfg.ProjectsDir,
		KeysDir:        cfg.DeployKeysDir,
		KnownHosts:     cfg.KnownHosts,
		Socket:         cfg.DockerSocket,
		AllowFileRepos: cfg.AllowFileRepos,
	}
	if path, err := exec.LookPath("git"); err == nil {
		engineCfg.Git = path
	}
	if path, err := exec.LookPath("docker"); err == nil {
		engineCfg.Docker = path
	}
	if cfg.DeployEnabled {
		log.InfoContext(ctx, "el deploy está prendido en este servidor", "git", engineCfg.Git, "docker", engineCfg.Docker)
	}
	engine, err := deploy.New(ctx, engineCfg, parts.dockerClient, projects.NewChecker(buildinfo.Version), projects.ReadSpec, out, log)
	if err != nil {
		return nil, fmt.Errorf("armar el motor de deploy: %w", err)
	}
	return engine, nil
}
