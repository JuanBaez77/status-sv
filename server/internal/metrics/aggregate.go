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

// Aggregator es lo que la agregación necesita de la base.
type Aggregator interface {
	AggregateSamplesByMinute(ctx context.Context, arg store.AggregateSamplesByMinuteParams) (int64, error)
	AggregateDisksByMinute(ctx context.Context, arg store.AggregateDisksByMinuteParams) (int64, error)
	AggregateNetworksByMinute(ctx context.Context, arg store.AggregateNetworksByMinuteParams) (int64, error)
	AggregateSamplesByHour(ctx context.Context, arg store.AggregateSamplesByHourParams) (int64, error)
	AggregateDisksByHour(ctx context.Context, arg store.AggregateDisksByHourParams) (int64, error)
	AggregateNetworksByHour(ctx context.Context, arg store.AggregateNetworksByHourParams) (int64, error)
}

// aggregate recalcula el agregado por minuto y después el agregado por hora
// de un servidor para las muestras que van de first a last (ADR-009).
//
// El minuto se recalcula siempre desde el crudo, y la hora desde el minuto,
// así que da igual si una muestra llegó tarde o repetida. La red abarca un
// poco más: una muestra que llega tarde cambia también la tasa de la muestra
// siguiente, que puede caer en otro minuto, y hasta en otra hora.
func aggregate(ctx context.Context, q Aggregator, serverID uuid.UUID, first, last time.Time) error {
	from, to := minuteSpan(first, last)
	_, networkTo := minuteSpan(first, last.Add(MaxRateGap))

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
	if _, err := q.AggregateNetworksByMinute(ctx, store.AggregateNetworksByMinuteParams{
		ServerID:      serverID,
		PrevFromTs:    from.Add(-MaxRateGap),
		FromTs:        from,
		ToTs:          networkTo,
		MaxGapSeconds: MaxRateGap.Seconds(),
	}); err != nil {
		return fmt.Errorf("agregar la red por minuto: %w", err)
	}

	hourFrom, hourTo := hourSpan(from, to)
	if _, err := q.AggregateSamplesByHour(ctx, store.AggregateSamplesByHourParams{
		ServerID: serverID, FromTs: hourFrom, ToTs: hourTo,
	}); err != nil {
		return fmt.Errorf("agregar las muestras por hora: %w", err)
	}
	if _, err := q.AggregateDisksByHour(ctx, store.AggregateDisksByHourParams{
		ServerID: serverID, FromTs: hourFrom, ToTs: hourTo,
	}); err != nil {
		return fmt.Errorf("agregar los discos por hora: %w", err)
	}
	networkHourFrom, networkHourTo := hourSpan(from, networkTo)
	if _, err := q.AggregateNetworksByHour(ctx, store.AggregateNetworksByHourParams{
		ServerID: serverID, FromTs: networkHourFrom, ToTs: networkHourTo,
	}); err != nil {
		return fmt.Errorf("agregar la red por hora: %w", err)
	}
	return nil
}

// minuteSpan devuelve los minutos enteros, en UTC, que cubren de first a last:
// desde el comienzo del minuto de first hasta el final del minuto de last.
func minuteSpan(first, last time.Time) (time.Time, time.Time) {
	return first.UTC().Truncate(time.Minute), last.UTC().Truncate(time.Minute).Add(time.Minute)
}

// hourSpan devuelve las horas enteras, en UTC, que cubren el tramo [from, to):
// desde el comienzo de la hora de from hasta el final de la hora del último
// instante antes de to.
func hourSpan(from, to time.Time) (time.Time, time.Time) {
	return from.UTC().Truncate(time.Hour), to.UTC().Add(-time.Nanosecond).Truncate(time.Hour).Add(time.Hour)
}
