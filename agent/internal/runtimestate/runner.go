package runtimestate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/JuanBaez77/status-sv/proto"
)

// Cuándo se toma una foto.
const (
	// DefaultSettle es el respiro después de un cambio: un deploy de Compose
	// para y arranca varios contenedores en un segundo, y así salen en una
	// sola foto.
	DefaultSettle = time.Second
	// DefaultRefresh es cada cuánto se toma una foto aunque no haya eventos: es
	// lo que trae el consumo al día y lo que ve los procesos, que no avisan.
	DefaultRefresh = 30 * time.Second
	// DefaultResend es cada cuánto sale una foto aunque sea igual a la
	// anterior, para que el control plane sepa que sigue valiendo.
	DefaultResend = 5 * time.Minute
)

// watcher es una fuente que avisa cuando cambia algo. Lo cumplen
// docker.Source y systemd.Source.
type watcher interface {
	Watch(ctx context.Context, changed func())
}

// Runner decide cuándo tomar una foto y la deja en la bandeja de salida.
type Runner struct {
	assembler *Assembler
	outbox    *Outbox
	watchers  []watcher
	log       *slog.Logger

	settle, refresh, resend time.Duration

	lastSig  [sha256.Size]byte
	lastSent time.Time
}

// NewRunner arma el Runner. watchers son las fuentes que avisan cambios.
func NewRunner(a *Assembler, o *Outbox, log *slog.Logger, watchers ...watcher) *Runner {
	return &Runner{
		assembler: a,
		outbox:    o,
		watchers:  watchers,
		log:       log,
		settle:    DefaultSettle,
		refresh:   DefaultRefresh,
		resend:    DefaultResend,
	}
}

// Run toma fotos hasta que se cancela ctx: una al arrancar, una después de
// cada tanda de cambios y una en cada refresco. Solo deja en la bandeja las
// que cambiaron, o una cada DefaultResend aunque no cambie nada.
func (r *Runner) Run(ctx context.Context) {
	changes := make(chan struct{}, 1)
	notify := func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	}

	var wg sync.WaitGroup
	for _, w := range r.watchers {
		wg.Go(func() { w.Watch(ctx, notify) })
	}
	defer wg.Wait()

	r.take(ctx)
	refresh := time.NewTicker(r.refresh)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-changes:
			// El respiro junta la ráfaga: lo que avise mientras tanto sale en
			// la misma foto.
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.settle):
			}
			select {
			case <-changes:
			default:
			}
			r.take(ctx)
		case <-refresh.C:
			r.take(ctx)
		}
	}
}

func (r *Runner) take(ctx context.Context) {
	state := r.assembler.Snapshot(ctx)
	sig := signature(state)
	if sig == r.lastSig && time.Since(r.lastSent) < r.resend {
		return
	}
	r.outbox.Put(state)
	r.lastSig, r.lastSent = sig, time.Now()
	r.log.DebugContext(ctx, "foto de runtime lista",
		"contenedores", len(state.Containers),
		"servicios", len(state.Services),
		"procesos", len(state.Processes),
	)
}

// signature resume lo que muestra una foto, sin la marca de tiempo y con el
// consumo redondeado (al 1% de CPU y al MiB de memoria): el consumo de un
// servidor tranquilo baila unas décimas todo el tiempo, y eso no justifica
// mandar otra foto.
func signature(s proto.RuntimeState) [sha256.Size]byte {
	s.TakenAt = time.Time{}
	s.Containers = cloneRounded(s.Containers, func(c *proto.Container) *proto.Usage { return c.Usage }, func(c *proto.Container, u *proto.Usage) { c.Usage = u })
	s.Services = cloneRounded(s.Services, func(v *proto.Service) *proto.Usage { return v.Usage }, func(v *proto.Service, u *proto.Usage) { v.Usage = u })
	s.Processes = cloneRounded(s.Processes, func(p *proto.Process) *proto.Usage { return &p.Usage }, func(p *proto.Process, u *proto.Usage) { p.Usage = *u })

	raw, err := json.Marshal(s)
	if err != nil {
		// No pasa con tipos del contrato; si pasara, la foto sale igual.
		return [sha256.Size]byte{}
	}
	return sha256.Sum256(raw)
}

func cloneRounded[T any](items []T, get func(*T) *proto.Usage, set func(*T, *proto.Usage)) []T {
	out := make([]T, len(items))
	copy(out, items)
	for i := range out {
		if u := get(&out[i]); u != nil {
			set(&out[i], &proto.Usage{
				CPUPercent:  math.Round(u.CPUPercent),
				MemoryBytes: u.MemoryBytes >> 20,
			})
		}
	}
	return out
}
