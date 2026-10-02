// Package deploys guarda los deploys que se piden desde el panel y el avance
// que manda el agent en deploy_event (ADR-015, ADR-017).
//
// La base garantiza lo importante: un solo deploy activo por proyecto (índice
// único parcial) y que una foto vieja, repetida o de otro servidor no pise la
// guardada (ApplyDeployEvent solo avanza con seq mayor y del servidor al que
// se le pidió).
package deploys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// PendingTimeout es cuánto puede quedar un deploy pendiente sin que el agent
// mande su primera foto. Pasado eso se cierra con no_answer.
const PendingTimeout = 2 * time.Minute

// uniqueViolation es el código de PostgreSQL para una violación de unicidad.
const uniqueViolation = "23505"

// Errores del servicio.
var (
	// ErrActive es que el proyecto ya tiene un deploy pendiente o en curso.
	ErrActive = errors.New("el proyecto ya tiene un deploy en curso")
	// ErrUnknownDeploy es una foto de un deploy que no existe, o que no es
	// del servidor que la manda.
	ErrUnknownDeploy = errors.New("no hay un deploy con ese ID para este servidor")
	// ErrClosed es una foto más nueva de un deploy que el agent ya había
	// cerrado. No se aplica: un final no se reabre ni se cambia por otro.
	ErrClosed = errors.New("el deploy ya estaba cerrado")
)

// Service guarda y actualiza deploys.
type Service struct {
	q   *store.Queries
	now func() time.Time
}

// NewService arma el servicio sobre la base.
func NewService(db store.DBTX) *Service {
	return &Service{q: store.New(db), now: time.Now}
}

// Request es un deploy pedido.
type Request struct {
	ID          uuid.UUID
	ServerID    uuid.UUID
	ProjectID   uuid.UUID
	ProjectName string
	Environment string
	Kind        proto.DeployKind
	// Commit vacío es la punta de la rama declarada.
	Commit   string
	UserID   uuid.UUID
	Username string
}

// Create anota un deploy pendiente. Si el proyecto ya tiene uno activo,
// devuelve ErrActive.
func (s *Service) Create(ctx context.Context, r Request) (store.GetDeployRow, error) {
	err := s.q.CreateDeploy(ctx, store.CreateDeployParams{
		ID: r.ID, ServerID: r.ServerID, ProjectID: &r.ProjectID, ProjectName: r.ProjectName,
		Environment: r.Environment, Kind: string(r.Kind), RequestedCommit: r.Commit,
		RequestedBy: &r.UserID, RequestedByName: r.Username, RequestedAt: s.now().UTC(),
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return store.GetDeployRow{}, ErrActive
	}
	if err != nil {
		return store.GetDeployRow{}, fmt.Errorf("anotar el deploy: %w", err)
	}
	return s.q.GetDeploy(ctx, r.ID)
}

// Reject cierra un deploy que el agent no aceptó o que no se le pudo mandar.
// Si ya no estaba pendiente (llegó una foto antes), no lo toca.
func (s *Service) Reject(ctx context.Context, id uuid.UUID, code, message string) (store.Deploy, bool, error) {
	d, err := s.q.RejectDeploy(ctx, store.RejectDeployParams{ID: id, Code: code, Message: clip(message, 1024), At: s.now().UTC()})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Deploy{}, false, nil
	}
	if err != nil {
		return store.Deploy{}, false, fmt.Errorf("cerrar el deploy rechazado: %w", err)
	}
	return d, true, nil
}

// Apply guarda una foto del agent. applied es false si la foto es más vieja
// o igual a la guardada (hay que confirmarla igual: ya se tiene algo mejor).
// Con ErrClosed tampoco se aplicó, y también hay que confirmarla: el agent no
// tiene nada mejor para mandar y no sirve que la reintente.
func (s *Service) Apply(ctx context.Context, serverID uuid.UUID, ev proto.DeployEvent) (store.Deploy, bool, error) {
	steps, err := json.Marshal(ev.Steps)
	if err != nil {
		return store.Deploy{}, false, fmt.Errorf("serializar los pasos: %w", err)
	}
	if ev.Steps == nil {
		steps = []byte("[]")
	}
	started := ev.StartedAt.UTC()
	var finished *time.Time
	if !ev.FinishedAt.IsZero() {
		f := ev.FinishedAt.UTC()
		finished = &f
	}
	d, err := s.q.ApplyDeployEvent(ctx, store.ApplyDeployEventParams{
		ID: ev.DeployID, ServerID: serverID, Status: string(ev.Status),
		DeployedCommit: ev.Commit, PreviousCommit: ev.PreviousCommit,
		Code: string(ev.Code), Message: clip(ev.Message, 1024),
		StartedAt: &started, FinishedAt: finished, Seq: int32(min(ev.Seq, 1<<30)), //nolint:gosec // acotado arriba
		Steps: steps, UpdatedAt: s.now().UTC(),
	})
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return d, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		seq, err := s.q.DeploySeq(ctx, store.DeploySeqParams{ID: ev.DeployID, ServerID: serverID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.Deploy{}, false, ErrUnknownDeploy
			}
			return store.Deploy{}, false, fmt.Errorf("buscar el deploy: %w", err)
		}
		if int(seq) < ev.Seq {
			return store.Deploy{}, false, ErrClosed
		}
		return store.Deploy{}, false, nil
	case errors.As(err, &pgErr) && pgErr.Code == uniqueViolation:
		// Un deploy que se había dado por perdido vuelve a estar activo
		// mientras corre otro del mismo proyecto. No se puede aplicar.
		return store.Deploy{}, false, ErrActive
	default:
		return store.Deploy{}, false, fmt.Errorf("guardar la foto del deploy: %w", err)
	}
}

// Expire cierra los deploys pendientes a los que el agent nunca contestó.
func (s *Service) Expire(ctx context.Context) ([]store.Deploy, error) {
	now := s.now().UTC()
	return s.q.ExpirePendingDeploys(ctx, store.ExpirePendingDeploysParams{Now: now, Before: now.Add(-PendingTimeout)})
}

// Get devuelve un deploy.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (store.GetDeployRow, error) {
	return s.q.GetDeploy(ctx, id)
}

// History devuelve los últimos deploys de un proyecto.
func (s *Service) History(ctx context.Context, serverID uuid.UUID, project string, limit int32) ([]store.ListProjectDeploysRow, error) {
	return s.q.ListProjectDeploys(ctx, store.ListProjectDeploysParams{ServerID: serverID, ProjectName: project, MaxRows: limit})
}

// Steps lee los pasos guardados. Si no se pueden leer (no debería pasar: los
// escribe Apply), devuelve una lista vacía.
func Steps(raw []byte) []proto.DeployStep {
	var steps []proto.DeployStep
	if err := json.Unmarshal(raw, &steps); err != nil || steps == nil {
		return []proto.DeployStep{}
	}
	return steps
}

// Final dice si un estado guardado es final.
func Final(status string) bool {
	return proto.DeployStatus(status).Final()
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
