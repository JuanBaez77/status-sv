// Package runstate guarda el estado de runtime que mandan los agents:
// contenedores, servicios y procesos no gestionados de cada servidor.
//
// Es el último estado conocido, no una historia (migración 00004). Cada foto
// reemplaza a la anterior, fuente por fuente, según lo que dice cada una de sí
// misma (proto/README.md).
package runstate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// MaxClockSkew es cuánto puede estar adelantado el reloj de un agent. Una foto
// más en el futuro quedaría como la última para siempre: ninguna foto correcta
// podría reemplazarla.
const MaxClockSkew = 10 * time.Minute

// ErrInvalidState es una foto que el control plane no va a aceptar nunca, por
// más que se reenvíe. El agent la descarta.
var ErrInvalidState = errors.New("estado de runtime inválido")

// Nombres de las fuentes y de los tipos de unidad, como quedan en la base.
const (
	SourceDocker    = "docker"
	SourceSystemd   = "systemd"
	SourceProcesses = "processes"

	KindContainer = "container"
	KindService   = "service"
	KindProcess   = "process"
)

// Beginner abre transacciones. Lo cumple *pgxpool.Pool.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Ingester guarda las fotos de runtime.
type Ingester struct {
	db  Beginner
	now func() time.Time
}

// NewIngester arma la ingesta.
func NewIngester(db Beginner) *Ingester {
	return &Ingester{db: db, now: time.Now}
}

// Ingest guarda una foto en una transacción: el panel nunca ve una foto a
// medias. Devuelve false si la foto era más vieja que la guardada y no se
// aplicó; igual hay que confirmarla, para que el agent deje de reenviarla.
//
// Para cada fuente:
//
//   - ok: su lista reemplaza a la anterior; lo que no aparece, se borra.
//   - unavailable: no existe en ese servidor; se borra lo que hubiera.
//   - error: se conserva lo último que se supo, con el aviso de la fuente.
//
// Si la foto es inválida devuelve un error que envuelve a ErrInvalidState; los
// demás errores son de la base, y conviene que el agent reintente.
func (i *Ingester) Ingest(ctx context.Context, serverID uuid.UUID, state proto.RuntimeState) (bool, error) {
	takenAt := state.TakenAt.UTC()
	if takenAt.After(i.now().Add(MaxClockSkew)) {
		return false, fmt.Errorf("%w: la foto es de %s, más de %s en el futuro; el reloj del servidor está mal",
			ErrInvalidState, takenAt.Format(time.RFC3339), MaxClockSkew)
	}

	parts := []struct {
		source  string
		kind    string
		status  proto.RuntimeSource
		omitted int
		units   columns
	}{
		{SourceDocker, KindContainer, state.Sources.Docker, state.Omitted.Containers, containerColumns(state.Containers)},
		{SourceSystemd, KindService, state.Sources.Systemd, state.Omitted.Services, serviceColumns(state.Services)},
		{SourceProcesses, KindProcess, state.Sources.Processes, state.Omitted.Processes, processColumns(state.Processes)},
	}
	for _, p := range parts {
		if err := checkMemory(p.units.MemoryBytes); err != nil {
			return false, fmt.Errorf("%w: %s: %w", ErrInvalidState, p.source, err)
		}
	}

	tx, err := i.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("abrir la transacción: %w", err)
	}
	// Después de un Commit exitoso, Rollback no hace nada.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	q := store.New(tx)
	advanced, err := q.AdvanceRuntimeTakenAt(ctx, store.AdvanceRuntimeTakenAtParams{ServerID: serverID, TakenAt: &takenAt})
	if err != nil {
		return false, fmt.Errorf("avanzar la marca de la foto: %w", err)
	}
	if advanced == 0 {
		return false, nil
	}

	for _, p := range parts {
		if err := q.UpsertRuntimeSource(ctx, store.UpsertRuntimeSourceParams{
			ServerID: serverID,
			Source:   p.source,
			Status:   string(p.status.Status),
			Detail:   p.status.Detail,
			Omitted:  int32(min(p.omitted, math.MaxInt32)), //nolint:gosec // acotado a int32 en la misma línea
			TakenAt:  takenAt,
		}); err != nil {
			return false, fmt.Errorf("guardar la fuente %s: %w", p.source, err)
		}

		switch p.status.Status {
		case proto.SourceOK:
			if _, err := q.DeleteRuntimeUnitsExcept(ctx, store.DeleteRuntimeUnitsExceptParams{
				ServerID: serverID, Kind: p.kind, Keep: p.units.UnitKey,
			}); err != nil {
				return false, fmt.Errorf("borrar lo que ya no está en %s: %w", p.source, err)
			}
			if len(p.units.UnitKey) > 0 {
				params := p.units.UpsertRuntimeUnitsParams
				params.ServerID, params.Kind, params.SeenAt = serverID, p.kind, takenAt
				if _, err := q.UpsertRuntimeUnits(ctx, params); err != nil {
					return false, fmt.Errorf("guardar las unidades de %s: %w", p.source, err)
				}
			}
		case proto.SourceUnavailable:
			if _, err := q.DeleteRuntimeUnitsExcept(ctx, store.DeleteRuntimeUnitsExceptParams{
				ServerID: serverID, Kind: p.kind, Keep: []string{},
			}); err != nil {
				return false, fmt.Errorf("borrar lo de la fuente %s: %w", p.source, err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("confirmar la transacción: %w", err)
	}
	return true, nil
}

// checkMemory revisa que los bytes entren en bigint.
func checkMemory(values []int64) error {
	for _, v := range values {
		if v < -1 {
			return errors.New("la memoria de una unidad no entra en la base")
		}
	}
	return nil
}

// columns arma los arreglos de una tanda de unidades, uno por columna.
type columns struct{ store.UpsertRuntimeUnitsParams }

func newColumns(n int) columns {
	return columns{store.UpsertRuntimeUnitsParams{
		UnitKey:        make([]string, 0, n),
		Name:           make([]string, 0, n),
		State:          make([]string, 0, n),
		Health:         make([]string, 0, n),
		LoadState:      make([]string, 0, n),
		SubState:       make([]string, 0, n),
		Result:         make([]string, 0, n),
		Image:          make([]string, 0, n),
		ComposeProject: make([]string, 0, n),
		ComposeService: make([]string, 0, n),
		Pid:            make([]int32, 0, n),
		UserName:       make([]string, 0, n),
		Command:        make([]string, 0, n),
		StartedAt:      make([]time.Time, 0, n),
		CpuPercent:     make([]float32, 0, n),
		MemoryBytes:    make([]int64, 0, n),
	}}
}

// unit son los valores de una fila. Lo que no aplica queda en su cero.
type unit struct {
	key, name, state, health, loadState, subState, result, image, project, service, user, command string

	pid       int
	startedAt time.Time
	usage     *proto.Usage
}

func (c *columns) add(u unit) {
	c.UnitKey = append(c.UnitKey, u.key)
	c.Name = append(c.Name, u.name)
	c.State = append(c.State, u.state)
	c.Health = append(c.Health, u.health)
	c.LoadState = append(c.LoadState, u.loadState)
	c.SubState = append(c.SubState, u.subState)
	c.Result = append(c.Result, u.result)
	c.Image = append(c.Image, u.image)
	c.ComposeProject = append(c.ComposeProject, u.project)
	c.ComposeService = append(c.ComposeService, u.service)
	c.Pid = append(c.Pid, int32(u.pid)) //nolint:gosec // el contrato acota el PID a 2^22
	c.UserName = append(c.UserName, u.user)
	c.Command = append(c.Command, u.command)
	// El año 1 y -1 son "no hay dato": la consulta los vuelve NULL.
	c.StartedAt = append(c.StartedAt, u.startedAt.UTC())
	cpu, memory := float32(-1), int64(-1)
	if u.usage != nil {
		cpu = float32(u.usage.CPUPercent)
		memory = -2 // fuera de rango hasta que se compruebe que entra
		if u.usage.MemoryBytes <= math.MaxInt64 {
			memory = int64(u.usage.MemoryBytes)
		}
	}
	c.CpuPercent = append(c.CpuPercent, cpu)
	c.MemoryBytes = append(c.MemoryBytes, memory)
}

func containerColumns(containers []proto.Container) columns {
	c := newColumns(len(containers))
	for _, ct := range containers {
		c.add(unit{
			key: ct.ID, name: ct.Name, state: string(ct.State), health: string(ct.Health),
			image: ct.Image, project: ct.ComposeProject, service: ct.ComposeService,
			startedAt: ct.StartedAt, usage: ct.Usage,
		})
	}
	return c
}

func serviceColumns(services []proto.Service) columns {
	c := newColumns(len(services))
	for _, s := range services {
		c.add(unit{
			key: s.Unit, name: s.Unit, state: string(s.ActiveState),
			loadState: s.LoadState, subState: s.SubState, result: s.Result,
			pid: s.MainPID, startedAt: s.ActiveSince, usage: s.Usage,
		})
	}
	return c
}

func processColumns(processes []proto.Process) columns {
	c := newColumns(len(processes))
	for _, p := range processes {
		usage := p.Usage
		c.add(unit{
			key: strconv.Itoa(p.PID), name: p.Name, state: "running",
			pid: p.PID, user: p.User, command: p.Command,
			startedAt: p.StartedAt, usage: &usage,
		})
	}
	return c
}
