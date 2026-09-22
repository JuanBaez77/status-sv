package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/collector"
)

// Rango aceptado para la espera entre las dos lecturas de CPU.
const (
	minSampleInterval = 100 * time.Millisecond
	maxSampleInterval = time.Minute
)

// runSample toma una muestra y la imprime como JSON, sin hablar con el control
// plane. Sirve para ver qué lee el agent en un servidor concreto, por ejemplo
// bajo el mismo aislamiento que le da systemd.
func runSample(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("sample", flag.ContinueOnError)
	interval := flags.Duration("interval", time.Second, "espera entre las dos lecturas de CPU")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *interval < minSampleInterval || *interval > maxSampleInterval {
		return fmt.Errorf("--interval tiene que estar entre %v y %v", minSampleInterval, maxSampleInterval)
	}

	return printSample(ctx, collector.New(), *interval, os.Stdout, os.Stderr)
}

// printSample hace las dos lecturas que necesita el CPU e imprime la segunda.
// Los avisos de una muestra incompleta van a stderr, para que stdout sea JSON
// válido siempre.
func printSample(ctx context.Context, c *collector.Collector, interval time.Duration, out, warnings io.Writer) error {
	if _, err := c.Collect(ctx); !errors.Is(err, collector.ErrNoBaseline) {
		if err == nil {
			err = errors.New("la primera lectura trajo una muestra y no debería")
		}
		return fmt.Errorf("primera lectura: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(interval):
	}

	sample, err := c.Collect(ctx)
	var partial *collector.PartialError
	switch {
	case errors.As(err, &partial):
		for _, problem := range partial.Problems {
			_, _ = fmt.Fprintf(warnings, "aviso: %v\n", problem)
		}
	case errors.Is(err, collector.ErrNoBaseline):
		return fmt.Errorf("los contadores de CPU no avanzaron en %v: probar con un --interval mayor", interval)
	case err != nil:
		return err
	}

	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(sample)
}
