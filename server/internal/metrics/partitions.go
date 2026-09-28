// Package metrics maneja las métricas de los servidores en el control plane.
// Por ahora, el mantenimiento de las particiones de sus tablas (ADR-008).
package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// Retención de cada nivel (docs/DISENO.md). Como se borra de a particiones
// enteras, lo guardado va de la retención a la retención más una partición:
// el crudo, de 48 a 72 horas; el agregado por minuto, de 30 días a dos meses, y
// el agregado por hora, de un año a un año y un mes.
const (
	RawRetention    = 48 * time.Hour
	MinuteRetention = 30 * 24 * time.Hour
	HourRetention   = 365 * 24 * time.Hour
)

// Cuánto por adelantado se crean las particiones. No hay partición por defecto
// (ADR-008), así que una muestra sin partición se rechaza: la anticipación
// cubre el reloj adelantado de un agent y un control plane que estuvo caído
// cuando cambió el día.
const (
	rawAhead    = 48 * time.Hour
	minuteAhead = 32 * 24 * time.Hour
	hourAhead   = 32 * 24 * time.Hour
)

// Frecuencia del mantenimiento.
const (
	DefaultEvery = time.Hour
	// DefaultRetry es la espera después de una falla, por ejemplo si el control
	// plane arrancó antes de que se aplicaran las migraciones.
	DefaultRetry = time.Minute
)

// policy es cómo se mantiene una tabla particionada.
type policy struct {
	table     string
	retention time.Duration
	ahead     time.Duration
}

func policies() []policy {
	return []policy{
		{table: "metrics_raw", retention: RawRetention, ahead: rawAhead},
		{table: "metrics_raw_disks", retention: RawRetention, ahead: rawAhead},
		{table: "metrics_raw_networks", retention: RawRetention, ahead: rawAhead},
		{table: "metrics_1m", retention: MinuteRetention, ahead: minuteAhead},
		{table: "metrics_1m_disks", retention: MinuteRetention, ahead: minuteAhead},
		{table: "metrics_1m_networks", retention: MinuteRetention, ahead: minuteAhead},
		{table: "metrics_1h", retention: HourRetention, ahead: hourAhead},
		{table: "metrics_1h_disks", retention: HourRetention, ahead: hourAhead},
		{table: "metrics_1h_networks", retention: HourRetention, ahead: hourAhead},
	}
}

// PartitionStore es lo que el mantenimiento necesita de la base.
type PartitionStore interface {
	EnsureMetricPartitions(ctx context.Context, arg store.EnsureMetricPartitionsParams) (int32, error)
	DropMetricPartitions(ctx context.Context, arg store.DropMetricPartitionsParams) (int32, error)
}

// Partitions crea por adelantado las particiones de las tablas de métricas y
// borra las vencidas.
type Partitions struct {
	store PartitionStore
	log   *slog.Logger

	now   func() time.Time
	every time.Duration
	retry time.Duration
}

// NewPartitions arma el mantenimiento.
func NewPartitions(s PartitionStore, log *slog.Logger) *Partitions {
	return &Partitions{
		store: s,
		log:   log,
		now:   time.Now,
		every: DefaultEvery,
		retry: DefaultRetry,
	}
}

// Result es lo que hizo una pasada de mantenimiento.
type Result struct {
	Created int
	Dropped int
}

// Maintain hace una pasada sobre todas las tablas de métricas. Una tabla que
// falla no frena a las demás: los errores vuelven juntos al final.
func (p *Partitions) Maintain(ctx context.Context) (Result, error) {
	now := p.now()

	var (
		result Result
		errs   []error
	)
	for _, pol := range policies() {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		created, err := p.store.EnsureMetricPartitions(ctx, store.EnsureMetricPartitionsParams{
			Parent: pol.table,
			FromTs: now.Add(-pol.retention),
			ToTs:   now.Add(pol.ahead),
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("crear las particiones de %s: %w", pol.table, err))
			continue
		}
		result.Created += int(created)

		dropped, err := p.store.DropMetricPartitions(ctx, store.DropMetricPartitionsParams{
			Parent:   pol.table,
			BeforeTs: now.Add(-pol.retention),
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("borrar las particiones vencidas de %s: %w", pol.table, err))
			continue
		}
		result.Dropped += int(dropped)
	}

	return result, errors.Join(errs...)
}

// Run mantiene las particiones hasta que se cancela el contexto: una pasada al
// arrancar y después una por hora. Si una pasada falla, se reintenta al minuto.
//
// Que falle no detiene al control plane: las particiones se crean con dos días
// de anticipación, así que hay margen de sobra para que alguien lo vea en el
// registro.
func (p *Partitions) Run(ctx context.Context) {
	for {
		wait := p.every

		result, err := p.Maintain(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			wait = p.retry
			p.log.WarnContext(ctx, "falló el mantenimiento de las particiones de métricas",
				"error", err, "reintento_en", wait)
		case result.Created > 0 || result.Dropped > 0:
			p.log.InfoContext(ctx, "particiones de métricas al día",
				"creadas", result.Created, "borradas", result.Dropped)
		default:
			p.log.DebugContext(ctx, "particiones de métricas al día, sin cambios")
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
