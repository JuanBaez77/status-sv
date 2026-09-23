// Command loadtest simula varios agents contra un control plane.
//
// Es la prueba de carga de docs/DISENO.md: con diez agents conectados, el
// control plane tiene que quedar por debajo de 100 MB de memoria. La memoria la
// mide el script que lo invoca (deploy/carga/run.sh); esto genera la carga y
// avisa si algún agent no pudo con lo suyo.
//
// No reimplementa el agent: usa su mismo cliente —enrolamiento, canal, latido,
// buffer y acuses— con un colector que inventa las lecturas en vez de leer
// /proc. Así lo que se mide es el camino de verdad, con la misma validación y
// la misma ingesta.
//
// Los tokens de enrolamiento se leen de la entrada estándar, uno por línea,
// para que no queden a la vista en la lista de procesos:
//
//	loadtest --url http://127.0.0.1:8098 --agents 10 --duration 2m < tokens
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
}

// options es lo que se puede pedir por la línea de comandos.
type options struct {
	url      string
	agents   int
	duration time.Duration
	disks    int
	networks int
	verbose  bool
}

func parseFlags(args []string, out io.Writer) (options, error) {
	var opts options

	flags := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	flags.SetOutput(out)
	flags.StringVar(&opts.url, "url", "http://127.0.0.1:8080", "dirección del control plane")
	flags.IntVar(&opts.agents, "agents", 10, "cuántos agents simular")
	flags.DurationVar(&opts.duration, "duration", 2*time.Minute, "cuánto dura la prueba")
	flags.IntVar(&opts.disks, "disks", 2, "discos por muestra")
	flags.IntVar(&opts.networks, "networks", 2, "interfaces de red por muestra")
	flags.BoolVar(&opts.verbose, "verbose", false, "registrar también lo que sale bien")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}

	switch {
	case opts.agents < 1:
		return options{}, errors.New("hace falta al menos un agent")
	case opts.duration <= 0:
		return options{}, errors.New("la duración tiene que ser positiva")
	case opts.disks < 0 || opts.networks < 0:
		return options{}, errors.New("los discos y las interfaces no pueden ser negativos")
	}
	return opts, nil
}

func (o options) level() slog.Level {
	if o.verbose {
		return slog.LevelDebug
	}
	// Por defecto solo se ve lo que salió mal, que es lo que interesa medir.
	return slog.LevelWarn
}

func run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	opts, err := parseFlags(args, out)
	if err != nil {
		return err
	}

	tokens, err := readTokens(in)
	if err != nil {
		return err
	}
	if len(tokens) < opts.agents {
		return fmt.Errorf("hacen falta %d tokens de enrolamiento y llegaron %d por la entrada estándar",
			opts.agents, len(tokens))
	}

	counts := &logCounts{}
	log := slog.New(newCountingHandler(
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: opts.level()}),
		counts,
	))

	agents := make([]*simulated, opts.agents)
	for i := range agents {
		agents[i] = newSimulated(i+1, tokens[i], opts)
	}

	say(out, "Enrolando %d agents en %s\n", opts.agents, opts.url)
	if err := enrollAll(ctx, agents, log, opts); err != nil {
		return err
	}

	say(out, "Midiendo durante %v\n", opts.duration)
	deadline, cancel := context.WithTimeout(ctx, opts.duration)
	defer cancel()

	failures := make([]error, opts.agents)
	var wg sync.WaitGroup
	for i, agent := range agents {
		wg.Go(func() {
			failures[i] = agent.serve(deadline, log)
		})
	}
	wg.Wait()

	return report(out, agents, failures, counts)
}

// enrollAll enrola a todos antes de medir, y falla si alguno no llega.
//
// El enrolamiento no entra en la medición: con todos los agents saliendo de la
// misma máquina hay que esperar turno, y ese tiempo no dice nada del control
// plane. Se le da un presupuesto generoso, proporcional a la cantidad de
// agents, porque el límite del control plane es por minuto.
func enrollAll(ctx context.Context, agents []*simulated, log *slog.Logger, opts options) error {
	budget := time.Minute + time.Duration(len(agents))*20*time.Second
	enrollCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	failures := make([]error, len(agents))
	var wg sync.WaitGroup
	for i, agent := range agents {
		wg.Go(func() {
			failures[i] = agent.enroll(enrollCtx, log)
		})
	}
	wg.Wait()

	if err := errors.Join(failures...); err != nil {
		return fmt.Errorf("no se pudieron enrolar los %d agents: %w", opts.agents, err)
	}
	return nil
}

// report imprime el resumen y decide si la prueba pasó.
//
// Que queden muestras sin confirmar al terminar es normal: el último lote puede
// estar en vuelo. Que se hayan descartado no: el buffer aguanta diez minutos, y
// llenarlo significa que el control plane no dio abasto.
func report(out io.Writer, agents []*simulated, failures []error, counts *logCounts) error {
	var generated, pending, dropped int
	for _, agent := range agents {
		generated += agent.collector.collected()
		pending += agent.pending()
		dropped += agent.dropped()
	}

	say(out, "\nResultado de la prueba de carga\n")
	say(out, "  muestras generadas        %d\n", generated)
	say(out, "  sin confirmar al cerrar   %d\n", pending)
	say(out, "  descartadas por el buffer %d\n", dropped)
	say(out, "  avisos                    %d\n", counts.warnings.Load())
	say(out, "  errores                   %d\n", counts.errors.Load())

	problems := make([]error, 0, len(failures))
	for _, err := range failures {
		if err != nil {
			problems = append(problems, err)
		}
	}

	switch {
	case len(problems) > 0:
		return fmt.Errorf("%d de %d agents fallaron: %w", len(problems), len(agents), errors.Join(problems...))
	case dropped > 0:
		return fmt.Errorf("se descartaron %d muestras: el control plane no dio abasto", dropped)
	case counts.errors.Load() > 0:
		return fmt.Errorf("hubo %d errores durante la prueba", counts.errors.Load())
	case generated == 0:
		return errors.New("no se generó ninguna muestra: la prueba no probó nada")
	}
	return nil
}

// say escribe en la salida del comando. Si la salida falla no hay nada que
// hacer con el error: el resultado igual se devuelve por el código de salida.
func say(out io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(out, format, args...)
}

// readTokens lee los tokens de enrolamiento, uno por línea. Las líneas vacías y
// las que empiezan con # se ignoran, para poder comentar el archivo.
func readTokens(in io.Reader) ([]string, error) {
	var tokens []string

	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		tokens = append(tokens, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("leer los tokens: %w", err)
	}
	if len(tokens) == 0 {
		return nil, errors.New("no llegó ningún token de enrolamiento por la entrada estándar")
	}
	return tokens, nil
}
