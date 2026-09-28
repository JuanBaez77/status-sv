package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// MaxClockSkew es cuánto puede estar adelantado el reloj de un agent. Una
// muestra con una marca más en el futuro no se guarda: en el panel quedaría
// fuera de lugar, y dice que el reloj del servidor está mal.
const MaxClockSkew = 10 * time.Minute

// ErrInvalidBatch es un lote que el control plane no va a aceptar nunca, por
// más que se reenvíe. El agent lo descarta.
var ErrInvalidBatch = errors.New("lote de métricas inválido")

// Beginner abre transacciones. Lo cumple *pgxpool.Pool.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Ingester guarda los lotes de métricas que mandan los agents.
type Ingester struct {
	db  Beginner
	now func() time.Time
}

// NewIngester arma la ingesta.
func NewIngester(db Beginner) *Ingester {
	return &Ingester{db: db, now: time.Now}
}

// IngestResult es lo que pasó con un lote.
type IngestResult struct {
	// Accepted son las muestras del lote que quedaron guardadas, nuevas o
	// porque ya estaban de un envío anterior.
	Accepted int
	// New son las que no estaban.
	New int
	// LastTS es la marca de tiempo más nueva del lote: el agent suelta de su
	// buffer todo lo que mandó hasta ahí.
	LastTS time.Time
}

// Ingest guarda un lote en las tres tablas del crudo, en una transacción: una
// muestra nunca queda guardada a medias. Guardar dos veces el mismo lote no
// duplica nada.
//
// En la misma transacción recalcula el agregado por minuto y por hora de lo que
// tocó el lote (ADR-009): el agregado nunca queda atrás del crudo, aunque una
// muestra llegue tarde. Si el lote no trajo nada nuevo, no hay nada que
// recalcular.
//
// Si el lote es inválido devuelve un error que envuelve a ErrInvalidBatch; los
// demás errores son de la base, y conviene que el agent reintente.
func (i *Ingester) Ingest(ctx context.Context, serverID uuid.UUID, samples []proto.Sample) (IngestResult, error) {
	batch, err := toColumns(serverID, samples, i.now())
	if err != nil {
		return IngestResult{}, err
	}

	tx, err := i.db.Begin(ctx)
	if err != nil {
		return IngestResult{}, fmt.Errorf("abrir la transacción: %w", err)
	}
	// Después de un Commit exitoso, Rollback no hace nada.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	q := store.New(tx)
	inserted, err := q.InsertRawSamples(ctx, batch.samples)
	if err != nil {
		return IngestResult{}, fmt.Errorf("guardar las muestras: %w", err)
	}
	var insertedDisks, insertedNetworks int64
	if len(batch.disks.Ts) > 0 {
		if insertedDisks, err = q.InsertRawDisks(ctx, batch.disks); err != nil {
			return IngestResult{}, fmt.Errorf("guardar los discos: %w", err)
		}
	}
	if len(batch.networks.Ts) > 0 {
		if insertedNetworks, err = q.InsertRawNetworks(ctx, batch.networks); err != nil {
			return IngestResult{}, fmt.Errorf("guardar las interfaces: %w", err)
		}
	}
	if inserted+insertedDisks+insertedNetworks > 0 {
		if err := aggregate(ctx, q, serverID, batch.firstTS, batch.lastTS); err != nil {
			return IngestResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return IngestResult{}, fmt.Errorf("confirmar la transacción: %w", err)
	}

	return IngestResult{Accepted: len(samples), New: int(inserted), LastTS: batch.lastTS}, nil
}

// columns es un lote ya pasado a los arreglos que reciben las consultas.
type columns struct {
	samples  store.InsertRawSamplesParams
	disks    store.InsertRawDisksParams
	networks store.InsertRawNetworksParams
	// firstTS y lastTS son la muestra más vieja y la más nueva del lote, que
	// puede venir en desorden.
	firstTS time.Time
	lastTS  time.Time
}

// toColumns revisa el lote y lo pasa a columnas. Lo que revisa acá es lo que el
// contrato no puede saber: la ventana de tiempo que tiene partición y los
// valores que no entran en las columnas de PostgreSQL.
func toColumns(serverID uuid.UUID, samples []proto.Sample, now time.Time) (columns, error) {
	if err := (proto.Metrics{Samples: samples}).Validate(); err != nil {
		return columns{}, fmt.Errorf("%w: %w", ErrInvalidBatch, err)
	}

	oldest, newest := now.Add(-RawRetention), now.Add(MaxClockSkew)
	c := columns{
		samples:  store.InsertRawSamplesParams{ServerID: serverID},
		disks:    store.InsertRawDisksParams{ServerID: serverID},
		networks: store.InsertRawNetworksParams{ServerID: serverID},
	}

	for i, sample := range samples {
		ts := sample.TS.UTC()
		switch {
		case ts.Before(oldest):
			return columns{}, fmt.Errorf("%w: la muestra %d es de %s, más vieja que las %v que se guardan: ¿el reloj del servidor está atrasado?",
				ErrInvalidBatch, i, ts.Format(time.RFC3339), RawRetention)
		case ts.After(newest):
			return columns{}, fmt.Errorf("%w: la muestra %d es de %s, en el futuro: el reloj del servidor está adelantado",
				ErrInvalidBatch, i, ts.Format(time.RFC3339))
		}
		if ts.After(c.lastTS) {
			c.lastTS = ts
		}
		if i == 0 || ts.Before(c.firstTS) {
			c.firstTS = ts
		}

		if err := appendSample(&c.samples, ts, sample); err != nil {
			return columns{}, fmt.Errorf("%w: muestra %d: %w", ErrInvalidBatch, i, err)
		}
		for _, disk := range sample.Disks {
			if err := appendDisk(&c.disks, ts, disk); err != nil {
				return columns{}, fmt.Errorf("%w: muestra %d, disco %q: %w", ErrInvalidBatch, i, disk.Mountpoint, err)
			}
		}
		for _, network := range sample.Networks {
			if err := appendNetwork(&c.networks, ts, network); err != nil {
				return columns{}, fmt.Errorf("%w: muestra %d, interfaz %q: %w", ErrInvalidBatch, i, network.Interface, err)
			}
		}
	}
	return c, nil
}

func appendSample(p *store.InsertRawSamplesParams, ts time.Time, s proto.Sample) error {
	cores, err := toSmallint(s.CPU.Cores)
	if err != nil {
		return fmt.Errorf("núcleos: %w", err)
	}
	memTotal, memUsed, err := bytesPair(s.Memory.TotalBytes, s.Memory.UsedBytes)
	if err != nil {
		return fmt.Errorf("memoria: %w", err)
	}
	swapTotal, swapUsed, err := bytesPair(s.Memory.SwapTotalBytes, s.Memory.SwapUsedBytes)
	if err != nil {
		return fmt.Errorf("swap: %w", err)
	}
	load1, load5, load15, err := loads(s.Load)
	if err != nil {
		return err
	}

	p.Ts = append(p.Ts, ts)
	p.CpuUsed = append(p.CpuUsed, float32(s.CPU.UsedPercent))
	p.CpuIowait = append(p.CpuIowait, float32(s.CPU.IOWaitPercent))
	p.CpuSteal = append(p.CpuSteal, float32(s.CPU.StealPercent))
	p.CpuCores = append(p.CpuCores, cores)
	p.MemTotal = append(p.MemTotal, memTotal)
	p.MemUsed = append(p.MemUsed, memUsed)
	p.SwapTotal = append(p.SwapTotal, swapTotal)
	p.SwapUsed = append(p.SwapUsed, swapUsed)
	p.Load1 = append(p.Load1, load1)
	p.Load5 = append(p.Load5, load5)
	p.Load15 = append(p.Load15, load15)
	return nil
}

func appendDisk(p *store.InsertRawDisksParams, ts time.Time, d proto.DiskSample) error {
	// El contrato ya garantiza usado ≤ total y disponible ≤ total − usado, así
	// que alcanza con que el total entre en bigint.
	total, err := toBigint(d.TotalBytes)
	if err != nil {
		return err
	}
	p.Ts = append(p.Ts, ts)
	p.Mountpoint = append(p.Mountpoint, d.Mountpoint)
	p.Total = append(p.Total, total)
	p.Used = append(p.Used, int64(d.UsedBytes))                //nolint:gosec // usado ≤ total, que ya entra en int64
	p.Available = append(p.Available, int64(d.AvailableBytes)) //nolint:gosec // disponible ≤ total, que ya entra en int64
	return nil
}

func appendNetwork(p *store.InsertRawNetworksParams, ts time.Time, n proto.NetworkSample) error {
	rx, err := toBigint(n.RxBytes)
	if err != nil {
		return fmt.Errorf("bytes recibidos: %w", err)
	}
	tx, err := toBigint(n.TxBytes)
	if err != nil {
		return fmt.Errorf("bytes enviados: %w", err)
	}
	p.Ts = append(p.Ts, ts)
	p.Interface = append(p.Interface, n.Interface)
	p.Rx = append(p.Rx, rx)
	p.Tx = append(p.Tx, tx)
	return nil
}

// bytesPair pasa un total y una parte (que el contrato garantiza menor o igual)
// a bigint.
func bytesPair(total, part uint64) (int64, int64, error) {
	t, err := toBigint(total)
	if err != nil {
		return 0, 0, err
	}
	return t, int64(part), nil //nolint:gosec // part ≤ total, que ya entra en int64
}

// toBigint pasa un contador sin signo a la columna bigint de PostgreSQL.
func toBigint(v uint64) (int64, error) {
	if v > math.MaxInt64 {
		return 0, fmt.Errorf("%d no entra en una columna bigint", v)
	}
	return int64(v), nil
}

// loads pasa la carga a real, la precisión de la columna. Una carga que se va
// de rango al achicarla no es una carga posible.
func loads(l proto.LoadSample) (float32, float32, float32, error) {
	values := [3]float32{float32(l.Load1), float32(l.Load5), float32(l.Load15)}
	for _, v := range values {
		if math.IsInf(float64(v), 0) {
			return 0, 0, 0, fmt.Errorf("carga imposible: %v, %v, %v", l.Load1, l.Load5, l.Load15)
		}
	}
	return values[0], values[1], values[2], nil
}

// toSmallint pasa un entero a la columna smallint de PostgreSQL.
func toSmallint(v int) (int16, error) {
	if v < math.MinInt16 || v > math.MaxInt16 {
		return 0, fmt.Errorf("%d no entra en una columna smallint", v)
	}
	return int16(v), nil
}
