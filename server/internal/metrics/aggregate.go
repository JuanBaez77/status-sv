package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// MaxRateGap es cuánto pueden estar separadas dos muestras de red para que su
// diferencia cuente como una tasa. Es el doble del intervalo de muestreo más
// largo que acepta el contrato (300 s): más que eso es un agent que estuvo
// caído, y promediar todo ese tiempo en un minuto daría una tasa que no pasó.
const MaxRateGap = 10 * time.Minute

// MinuteAggregator es lo que la agregación necesita de la base.
type MinuteAggregator interface {
	AggregateSamplesByMinute(ctx context.Context, arg store.AggregateSamplesByMinuteParams) (int64, error)
	AggregateDisksByMinute(ctx context.Context, arg store.AggregateDisksByMinuteParams) (int64, error)
	AggregateNetworksByMinute(ctx context.Context, arg store.AggregateNetworksByMinuteParams) (int64, error)
}

// aggregateMinutes recalcula el agregado por minuto de un servidor para las
// muestras que van de first a last (ADR-009).
//
// Se recalcula siempre desde el crudo, así que da igual si una muestra llegó
// tarde o repetida. La red abarca un poco más: una muestra que llega tarde
// cambia también la tasa de la muestra siguiente, que puede caer en otro
// minuto.
func aggregateMinutes(ctx context.Context, q MinuteAggregator, serverID uuid.UUID, first, last time.Time) error {
	from, to := minuteSpan(first, last)

	if _, err := q.AggregateSamplesByMinute(ctx, store.AggregateSamplesByMinuteParams{
		ServerID: serverID, FromTs: from, ToTs: to,
	}); err != nil {
		return fmt.Errorf("agregar las muestras por minuto: %w", err)
	}
	if _, err := q.AggregateDisksByMinute(ctx, store.AggregateDisksByMinuteParams{
		ServerID: serverID, FromTs: from, ToTs: to,
	}); err != nil {
		return fmt.Errorf("agregar los discos por minuto: %w", err)
	}

	_, networkTo := minuteSpan(first, last.Add(MaxRateGap))
	if _, err := q.AggregateNetworksByMinute(ctx, store.AggregateNetworksByMinuteParams{
		ServerID:      serverID,
		PrevFromTs:    from.Add(-MaxRateGap),
		FromTs:        from,
		ToTs:          networkTo,
		MaxGapSeconds: MaxRateGap.Seconds(),
	}); err != nil {
		return fmt.Errorf("agregar la red por minuto: %w", err)
	}
	return nil
}

// minuteSpan devuelve los minutos enteros, en UTC, que cubren de first a last:
// desde el comienzo del minuto de first hasta el final del minuto de last.
func minuteSpan(first, last time.Time) (time.Time, time.Time) {
	return first.UTC().Truncate(time.Minute), last.UTC().Truncate(time.Minute).Add(time.Minute)
}
