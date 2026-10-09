// Package projects guarda los proyectos declarados que mandan los agents
// (ADR-012) y los arma para el panel.
//
// El control plane no crea ni cambia proyectos: guarda la última lectura que
// hizo el agent de su directorio. Es el último estado conocido, no una
// historia (migración 00008).
package projects

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

// MaxClockSkew es cuánto puede estar adelantado el reloj de un agent, igual
// que con las fotos de runtime.
const MaxClockSkew = 10 * time.Minute

// ErrInvalidState es una lectura que el control plane no va a aceptar nunca.
// El agent la descarta.
var ErrInvalidState = errors.New("proyectos declarados inválidos")

// Beginner abre transacciones. Lo cumple *pgxpool.Pool.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Ingester guarda las lecturas de proyectos.
type Ingester struct {
	db  Beginner
	now func() time.Time
}

// NewIngester arma la ingesta.
func NewIngester(db Beginner) *Ingester {
	return &Ingester{db: db, now: time.Now}
}

// Ingest guarda una lectura en una transacción. Devuelve false si era más vieja
// que la guardada y no se aplicó; igual hay que confirmarla, para que el agent
// deje de reenviarla.
//
// Según cómo salió leer el directorio:
//
//   - ok: la lista reemplaza a la anterior; lo que no aparece, se borra.
//   - unavailable: el servidor no tiene proyectos; se borra lo que hubiera.
//   - error: se conserva lo último que se supo, con el aviso.
func (i *Ingester) Ingest(ctx context.Context, serverID uuid.UUID, state proto.ProjectsState) (bool, error) {
	takenAt := state.TakenAt.UTC()
	if takenAt.After(i.now().Add(MaxClockSkew)) {
		return false, fmt.Errorf("%w: la lectura es de %s, más de %s en el futuro; el reloj del servidor está mal",
			ErrInvalidState, takenAt.Format(time.RFC3339), MaxClockSkew)
	}

	tx, err := i.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("abrir la transacción: %w", err)
	}
	// Después de un Commit exitoso, Rollback no hace nada.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	q := store.New(tx)
	advanced, err := q.AdvanceProjectsTakenAt(ctx, store.AdvanceProjectsTakenAtParams{
		ServerID: serverID,
		TakenAt:  &takenAt,
		Status:   string(state.Source.Status),
		Detail:   state.Source.Detail,
	})
	if err != nil {
		return false, fmt.Errorf("avanzar la marca de los proyectos: %w", err)
	}
	if advanced == 0 {
		return false, nil
	}

	switch state.Source.Status {
	case proto.SourceOK:
		if err := replace(ctx, q, serverID, takenAt, state.Projects, state.Invalid); err != nil {
			return false, err
		}
	case proto.SourceUnavailable:
		if err := replace(ctx, q, serverID, takenAt, nil, nil); err != nil {
			return false, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("confirmar la transacción: %w", err)
	}
	return true, nil
}

// replace deja en la base exactamente los proyectos y archivos inválidos de
// una lectura.
func replace(ctx context.Context, q *store.Queries, serverID uuid.UUID, seenAt time.Time, declared []proto.Project, invalid []proto.InvalidProject) error {
	keep := make([]string, 0, len(declared))
	for _, p := range declared {
		keep = append(keep, p.Name)
	}
	if _, err := q.DeleteProjectsExcept(ctx, store.DeleteProjectsExceptParams{ServerID: serverID, Keep: keep}); err != nil {
		return fmt.Errorf("borrar los proyectos que ya no están: %w", err)
	}
	for _, p := range declared {
		params, err := upsertParams(serverID, seenAt, p)
		if err != nil {
			return err
		}
		if err := q.UpsertProject(ctx, params); err != nil {
			return fmt.Errorf("guardar el proyecto %s: %w", p.Name, err)
		}
	}

	if _, err := q.DeleteInvalidProjectFiles(ctx, serverID); err != nil {
		return fmt.Errorf("borrar los archivos inválidos anteriores: %w", err)
	}
	if len(invalid) > 0 {
		files := make([]string, len(invalid))
		reasons := make([]string, len(invalid))
		for i, f := range invalid {
			files[i], reasons[i] = f.File, f.Error
		}
		if _, err := q.InsertInvalidProjectFiles(ctx, store.InsertInvalidProjectFilesParams{
			ServerID: serverID, Files: files, Errors: reasons, SeenAt: seenAt,
		}); err != nil {
			return fmt.Errorf("guardar los archivos inválidos: %w", err)
		}
	}
	return nil
}

func upsertParams(serverID uuid.UUID, seenAt time.Time, p proto.Project) (store.UpsertProjectParams, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return store.UpsertProjectParams{}, fmt.Errorf("generar el ID: %w", err)
	}
	params := store.UpsertProjectParams{
		ID:             id,
		ServerID:       serverID,
		Name:           p.Name,
		Environment:    string(p.Environment),
		Repo:           p.Repo,
		Branch:         p.Branch,
		Runtime:        string(p.Runtime),
		Workdir:        p.Workdir,
		ComposeFile:    p.ComposeFile,
		ComposeProject: p.ComposeProject,
		Units:          p.Units,
		SeenAt:         seenAt,
		EnvMissing:     []string{},
		DeployKey:      p.DeployKey,
		SpecHash:       p.SpecHash,
	}
	if params.Units == nil {
		params.Units = []string{}
	}
	if s := p.Site; s != nil {
		params.SiteDomain, params.SitePort = s.Domain, toInt32(s.Port)
	}
	// De las variables llegan cuántas son y los nombres de las que faltan,
	// nunca valores (ADR-022).
	if e := p.Env; e != nil {
		params.EnvRequired, params.EnvUnknown = toInt32(e.Required), e.Unknown
		if e.Missing != nil {
			params.EnvMissing = e.Missing
		}
	}
	if h := p.Healthcheck; h != nil {
		params.HealthcheckUrl = h.URL
		params.HealthcheckExpectStatus = toInt32(h.ExpectStatus)
		params.HealthcheckTimeoutSeconds = toInt32(h.TimeoutSeconds)
	}
	if h := p.Health; h != nil {
		checked := h.CheckedAt.UTC()
		params.HealthState = string(h.State)
		params.HealthCheckedAt = &checked
		params.HealthStatusCode = toInt32(h.StatusCode)
		params.HealthLatencyMs = toInt32(h.LatencyMS)
		params.HealthDetail = h.Detail
	}
	return params, nil
}

// toInt32 pasa a int32 un valor que el contrato ya acotó muy por debajo.
func toInt32(v int) int32 {
	return int32(max(min(v, math.MaxInt32), math.MinInt32)) //nolint:gosec // acotado en la misma línea
}
