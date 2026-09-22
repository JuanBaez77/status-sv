package metrics

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/collector"
	"github.com/JuanBaez77/status-sv/proto"
)

// Collector es lo que el muestreo necesita del colector. Es una interfaz para
// poder probar el muestreo sin /proc.
type Collector interface {
	Collect(ctx context.Context) (proto.Sample, error)
}

// Sampler toma una muestra por intervalo y la deja en el buffer.
type Sampler struct {
	collector Collector
	buffer    *Buffer
	// interval se consulta en cada vuelta, porque el control plane puede
	// cambiarlo en cualquier momento.
	interval func() time.Duration
	log      *slog.Logger

	// Estado para no repetir el mismo aviso cada 10 segundos.
	lastProblem string
	overflowing bool
}

// NewSampler arma el muestreo.
func NewSampler(c Collector, buffer *Buffer, interval func() time.Duration, log *slog.Logger) *Sampler {
	return &Sampler{collector: c, buffer: buffer, interval: interval, log: log}
}

// Run muestrea hasta que se cancela el contexto.
func (s *Sampler) Run(ctx context.Context) {
	current := s.interval()
	ticker := time.NewTicker(current)
	defer ticker.Stop()

	// La primera lectura solo deja la base contra la cual calcular el CPU: la
	// primera muestra real sale a un intervalo del arranque.
	s.sample(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		s.sample(ctx)

		if next := s.interval(); next != current {
			s.log.InfoContext(ctx, "cambió el intervalo de muestreo", "antes", current, "ahora", next)
			ticker.Reset(next)
			current = next
		}
	}
}

// sample toma una muestra y la guarda. Nada de lo que pase acá corta el
// muestreo: un error se registra y se vuelve a intentar en el intervalo
// siguiente.
func (s *Sampler) sample(ctx context.Context) {
	sample, err := s.collector.Collect(ctx)

	var partial *collector.PartialError
	switch {
	case ctx.Err() != nil:
		return
	case errors.Is(err, collector.ErrNoBaseline):
		s.log.DebugContext(ctx, "lectura de base del CPU, sin muestra todavía", "detalle", err)
		return
	case errors.As(err, &partial):
		// La muestra sirve igual; se avisa lo que le falta.
		s.problem(ctx, err)
	case err != nil:
		s.problem(ctx, err)
		return
	default:
		s.recovered(ctx)
	}

	dropped := s.buffer.Add(sample)
	switch {
	case dropped > 0 && !s.overflowing:
		s.overflowing = true
		s.log.WarnContext(ctx, "el buffer de métricas está lleno: se descartan las muestras más viejas hasta que vuelva el canal",
			"pendientes", s.buffer.Len())
	case dropped == 0 && s.overflowing:
		s.overflowing = false
		s.log.InfoContext(ctx, "el buffer de métricas dejó de descartar",
			"perdidas_desde_el_arranque", s.buffer.Dropped())
	}
}

// problem registra un problema del colector. El mismo problema se repite en
// cada muestra, así que solo el primero va como aviso.
func (s *Sampler) problem(ctx context.Context, err error) {
	if err.Error() == s.lastProblem {
		s.log.DebugContext(ctx, "el colector sigue con el mismo problema", "error", err)
		return
	}
	s.lastProblem = err.Error()
	s.log.WarnContext(ctx, "problema al tomar la muestra", "error", err)
}

func (s *Sampler) recovered(ctx context.Context) {
	if s.lastProblem != "" {
		s.lastProblem = ""
		s.log.InfoContext(ctx, "las muestras vuelven a salir completas")
	}
}
