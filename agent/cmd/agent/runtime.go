package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	goruntime "runtime"
	"slices"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/cgroups"
	"github.com/JuanBaez77/status-sv/agent/internal/config"
	"github.com/JuanBaez77/status-sv/agent/internal/docker"
	"github.com/JuanBaez77/status-sv/agent/internal/procs"
	"github.com/JuanBaez77/status-sv/agent/internal/runtimestate"
	"github.com/JuanBaez77/status-sv/agent/internal/systemd"
)

// runRuntime arma una foto de runtime (contenedores, servicios y procesos no
// gestionados) y la imprime como JSON, sin hablar con el control plane. Es a
// la foto lo que sample es a las métricas: sirve para ver qué ve el agent en
// un servidor concreto, bajo el mismo aislamiento que le da systemd.
func runRuntime(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("runtime", flag.ContinueOnError)
	interval := flags.Duration("interval", time.Second, "espera entre las dos lecturas de consumo")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *interval < minSampleInterval || *interval > maxSampleInterval {
		return fmt.Errorf("--interval tiene que estar entre %v y %v", minSampleInterval, maxSampleInterval)
	}
	if goruntime.GOOS != "linux" {
		return errors.New("el estado de runtime solo se lee en Linux")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Los avisos de las fuentes van a stderr, para que stdout sea JSON válido.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	parts := newRuntimeParts(cfg, log)
	defer parts.systemdSource.Close()
	return printRuntime(ctx, parts.assembler, *interval, os.Stdout)
}

// runtimeParts son las piezas del estado de runtime sobre el sistema de
// verdad. Las comparten run y este subcomando, así ven exactamente lo mismo.
type runtimeParts struct {
	assembler     *runtimestate.Assembler
	dockerClient  *docker.Client
	dockerSource  *docker.Source
	systemdSource *systemd.Source
}

func newRuntimeParts(cfg config.Config, log *slog.Logger) runtimeParts {
	cores := goruntime.NumCPU()
	dockerClient := docker.NewClient(cfg.DockerSocket)
	dockerSource := docker.NewSource(dockerClient, log)
	systemdSource := systemd.NewSource(log, append(slices.Clone(systemd.DefaultExclude), cfg.SystemdExclude...))
	scanner := procs.NewScanner(os.DirFS("/proc"), os.DirFS("/etc"), cores, os.Getpagesize())
	reader := cgroups.NewReader(os.DirFS(cgroups.Root))
	return runtimeParts{
		assembler:     runtimestate.NewAssembler(dockerSource, systemdSource, scanner, reader, cores, log),
		dockerClient:  dockerClient,
		dockerSource:  dockerSource,
		systemdSource: systemdSource,
	}
}

// printRuntime toma dos fotos, para que la segunda traiga el consumo, e
// imprime la segunda.
func printRuntime(ctx context.Context, a *runtimestate.Assembler, interval time.Duration, out io.Writer) error {
	a.Snapshot(ctx)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(interval):
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(a.Snapshot(ctx))
}
