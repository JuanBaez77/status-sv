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
	assembler, _, systemdSource := newAssembler(cfg, log)
	defer systemdSource.Close()
	return printRuntime(ctx, assembler, *interval, os.Stdout)
}

// newAssembler arma el ensamblador sobre el sistema de verdad. Lo comparten
// run y este subcomando, así ven exactamente lo mismo.
func newAssembler(cfg config.Config, log *slog.Logger) (*runtimestate.Assembler, *docker.Source, *systemd.Source) {
	cores := goruntime.NumCPU()
	dockerSource := docker.NewSource(docker.NewClient(cfg.DockerSocket), log)
	systemdSource := systemd.NewSource(log, append(slices.Clone(systemd.DefaultExclude), cfg.SystemdExclude...))
	scanner := procs.NewScanner(os.DirFS("/proc"), os.DirFS("/etc"), cores, os.Getpagesize())
	reader := cgroups.NewReader(os.DirFS(cgroups.Root))
	return runtimestate.NewAssembler(dockerSource, systemdSource, scanner, reader, cores, log), dockerSource, systemdSource
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
