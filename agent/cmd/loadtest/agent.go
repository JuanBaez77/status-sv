package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/metrics"
	"github.com/JuanBaez77/status-sv/agent/internal/runtimestate"
	"github.com/JuanBaez77/status-sv/agent/internal/state"
	"github.com/JuanBaez77/status-sv/agent/internal/ws"
	"github.com/JuanBaez77/status-sv/internal/buildinfo"
)

// enrollRetry es cuánto se espera antes de volver a pedir turno para enrolar.
//
// El control plane acepta unos pocos enrolamientos por minuto desde la misma
// IP: los tokens son de un solo uso y el límite está para que nadie los adivine
// a fuerza bruta. En producción cada agent viene de su propio VPS y nunca lo
// toca; acá salen todos de la misma máquina, así que hay que hacer la cola.
const enrollRetry = 5 * time.Second

// simulated es un agent de mentira: el cliente de verdad, con un colector que
// inventa las lecturas en vez de leer /proc.
type simulated struct {
	name      string
	number    int
	token     string
	opts      options
	buffer    *metrics.Buffer
	collector *syntheticCollector

	enrolled state.State
}

func newSimulated(number int, token string, opts options) *simulated {
	return &simulated{
		name:      fmt.Sprintf("carga-%02d", number),
		number:    number,
		token:     token,
		opts:      opts,
		buffer:    metrics.NewBuffer(metrics.DefaultMaxAge, metrics.DefaultMaxSamples),
		collector: newSyntheticCollector(number, opts.disks, opts.networks),
	}
}

// enroll canjea el token por la credencial del agent, esperando turno si el
// control plane está limitando los enrolamientos.
//
// Se hace antes de empezar a medir: el tiempo que tarda la cola no es parte de
// lo que se quiere medir.
func (s *simulated) enroll(ctx context.Context, log *slog.Logger) error {
	for {
		saved, err := ws.Enroll(ctx, s.opts.url, s.token, s.name+".simulado",
			buildinfo.Version, "linux", "amd64")
		if err == nil {
			s.enrolled = saved
			return nil
		}

		// Se reintenta el límite de frecuencia y también un corte de conexión:
		// con diez enrolamientos seguidos, el cliente HTTP puede reusar una
		// conexión que el control plane acaba de cerrar, y un POST no se
		// reintenta solo. Un rechazo definitivo (token inválido) no se reintenta.
		var rejected *ws.ErrEnrollRejected
		isRejection := errors.As(err, &rejected)
		switch {
		case ctx.Err() != nil:
			return fmt.Errorf("%s: enrolar: se acabó el tiempo: %w", s.name, err)
		case isRejection && !rejected.Retryable():
			return fmt.Errorf("%s: enrolar: %w", s.name, err)
		}

		log.DebugContext(ctx, "esperando turno para enrolar", "simulado", s.name, "motivo", err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: enrolar: se acabó el tiempo: %w", s.name, err)
		case <-time.After(enrollRetry):
		}
	}
}

// serve mantiene el canal y el muestreo hasta que se acaba el tiempo.
func (s *simulated) serve(ctx context.Context, log *slog.Logger) error {
	log = log.With("simulado", s.name)

	client := ws.NewClient(s.enrolled, buildinfo.Version, log, s.buffer)
	// El muestreo va aparte del canal, igual que en el agent de verdad: si la
	// conexión se corta, las muestras siguen entrando al buffer.
	sampler := metrics.NewSampler(s.collector, s.buffer, client.MetricsInterval, log)

	var wg sync.WaitGroup
	wg.Go(func() { sampler.Run(ctx) })

	// Las fotos de runtime, para que la carga incluya ese tráfico también.
	outbox := runtimestate.NewOutbox()
	client.SetRuntime(outbox)
	wg.Go(func() { feedRuntime(ctx, s.number, outbox) })
	err := client.Run(ctx)
	wg.Wait()

	if err != nil {
		return fmt.Errorf("%s: %w", s.name, err)
	}
	return nil
}

func (s *simulated) pending() int { return s.buffer.Len() }
func (s *simulated) dropped() int { return s.buffer.Dropped() }
