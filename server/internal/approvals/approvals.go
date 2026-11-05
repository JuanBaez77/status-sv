// Package approvals guarda los pedidos de escritura del MCP y su decisión
// (ADR-026): los crea, los aprueba o los rechaza, los vence y los cancela.
//
// El paquete no ejecuta nada. Aprobar solo cambia el estado del pedido: lo que
// se le manda al agent lo decide la API, que antes vuelve a verificar el token,
// el proyecto y el agent. Lo que se ejecuta sale de los campos tipados del
// pedido, nunca de Reason, que es texto del LLM.
package approvals

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// Límites de un pedido. La migración 00014 exige los mismos.
const (
	// TTL es cuánto espera un pedido a que alguien lo decida.
	TTL = time.Hour
	// MaxReasonLen es el largo máximo del motivo del LLM, en caracteres.
	MaxReasonLen = 500
	// MaxNoteLen es el largo máximo del motivo de un rechazo o una
	// cancelación, en caracteres.
	MaxNoteLen = 512
	// DefaultListLimit y MaxListLimit acotan el listado.
	DefaultListLimit = 50
	MaxListLimit     = 200

	maxUnitLen    = 255
	maxNameLen    = 64
	maxProjectLen = 63
	maxIPLen      = 64
)

// uniqueViolation es el código de PostgreSQL para una violación de unicidad.
const uniqueViolation = "23505"

// Errores del paquete.
var (
	// ErrInvalid envuelve los errores de un pedido mal armado: el mensaje se
	// le puede devolver a quien lo pidió.
	ErrInvalid = errors.New("pedido inválido")
	// ErrNotFound es un ID que no corresponde a ningún pedido.
	ErrNotFound = errors.New("el pedido no existe")
	// ErrPendingExists es un pedido sobre un proyecto que ya tiene uno
	// esperando. El error concreto es *PendingExistsError, que lo nombra.
	ErrPendingExists = errors.New("ya hay un pedido pendiente para este proyecto")
	// ErrNotPending es decidir un pedido que ya se decidió, se canceló o
	// venció.
	ErrNotPending = errors.New("el pedido ya no está pendiente")
)

// PendingExistsError nombra el pedido que ya espera sobre el proyecto.
type PendingExistsError struct {
	ID uuid.UUID
}

func (e *PendingExistsError) Error() string {
	return fmt.Sprintf("%s: %s", ErrPendingExists, e.ID)
}

// Unwrap deja preguntar con errors.Is(err, ErrPendingExists).
func (e *PendingExistsError) Unwrap() error { return ErrPendingExists }

// Operation es lo que se pide.
type Operation string

// Operaciones que se pueden pedir (ADR-026).
const (
	OpDeploy    Operation = "deploy"
	OpRollback  Operation = "rollback"
	OpRestart   Operation = "restart"
	OpBootstrap Operation = "bootstrap"
)

// Status es el estado de un pedido.
type Status string

// Estados de un pedido. Uno decidido no cambia más.
const (
	StatusPending   Status = "pending"
	StatusApproved  Status = "approved"
	StatusRejected  Status = "rejected"
	StatusExpired   Status = "expired"
	StatusCancelled Status = "cancelled"
)

// ValidStatus dice si s es uno de los estados.
func ValidStatus(s Status) bool {
	switch s {
	case StatusPending, StatusApproved, StatusRejected, StatusExpired, StatusCancelled:
		return true
	}
	return false
}

// Approval es un pedido de escritura.
type Approval struct {
	ID         uuid.UUID
	Operation  Operation
	ServerID   uuid.UUID
	ServerName string
	// ProjectID es nil si el proyecto dejó de estar declarado.
	ProjectID   *uuid.UUID
	ProjectName string
	Environment string
	// Commit es el SHA completo; solo en un deploy.
	Commit string
	// Unit es la unidad a reiniciar; solo en un restart, y vacía es todas.
	Unit string
	// Reason es el motivo que dio el LLM: texto no confiable.
	Reason string
	// RequestedByToken es nil si la fila del token ya no está.
	RequestedByToken  *uuid.UUID
	RequestedByName   string
	RequestedByPrefix string
	RequestedIP       string
	RequestedAt       time.Time
	ExpiresAt         time.Time
	Status            Status
	DecidedAt         *time.Time
	// DecidedByName es quien lo aprobó o lo rechazó; vacío si lo venció o lo
	// canceló el sistema.
	DecidedByName string
	DecisionNote  string
	DeployID      *uuid.UUID
}

// Request es un pedido nuevo, ya resuelto contra el token y el proyecto.
type Request struct {
	Operation   Operation
	ServerID    uuid.UUID
	ProjectID   uuid.UUID
	ProjectName string
	Environment string
	Commit      string
	Unit        string
	Reason      string
	TokenID     uuid.UUID
	TokenName   string
	TokenPrefix string
	IP          string
}

// Decider es la persona que aprueba o rechaza.
type Decider struct {
	UserID uuid.UUID
	Name   string
}

// Service guarda y decide pedidos.
type Service struct {
	q   *store.Queries
	now func() time.Time
}

// NewService arma el servicio sobre la base.
func NewService(db store.DBTX) *Service {
	return &Service{q: store.New(db), now: time.Now}
}

// unitPattern es un nombre de contenedor, de servicio de Compose o de unidad
// de systemd. La API después exige que sea del proyecto.
var unitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:-]*$`)

// prefixPattern es el prefijo guardado de un token del MCP.
var prefixPattern = regexp.MustCompile(`^svmcp_[A-Za-z0-9_-]{6}$`)

// validate revisa un pedido y devuelve su motivo normalizado.
func validate(r Request) (string, error) {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
	}
	switch r.Operation {
	case OpDeploy, OpRollback, OpRestart, OpBootstrap:
	default:
		return "", invalid("operación desconocida %q", r.Operation)
	}
	if r.Environment != string(proto.EnvProduction) && r.Environment != string(proto.EnvStaging) {
		return "", invalid("entorno desconocido %q", r.Environment)
	}
	if r.ServerID == uuid.Nil || r.ProjectID == uuid.Nil || r.TokenID == uuid.Nil {
		return "", invalid("falta el servidor, el proyecto o el token")
	}
	if n := utf8.RuneCountInString(r.ProjectName); n < 1 || n > maxProjectLen {
		return "", invalid("el nombre del proyecto no es válido")
	}
	if n := utf8.RuneCountInString(r.TokenName); n < 1 || n > maxNameLen {
		return "", invalid("el nombre del token no es válido")
	}
	if !prefixPattern.MatchString(r.TokenPrefix) {
		return "", invalid("el prefijo del token no es válido")
	}

	switch {
	case r.Operation == OpDeploy && !proto.ValidCommit(r.Commit):
		return "", invalid("un deploy lleva el commit completo: 40 caracteres hexadecimales en minúscula")
	case r.Operation != OpDeploy && r.Commit != "":
		return "", invalid("solo un deploy lleva commit")
	}
	switch {
	case r.Unit != "" && r.Operation != OpRestart:
		return "", invalid("solo un restart lleva unidad")
	case r.Unit != "" && (len(r.Unit) > maxUnitLen || !unitPattern.MatchString(r.Unit)):
		return "", invalid("la unidad %q no es un nombre válido", r.Unit)
	}

	reason, err := cleanText(r.Reason, MaxReasonLen)
	if err != nil {
		return "", invalid("el motivo %s", err)
	}
	return reason, nil
}

// cleanText cambia los caracteres de control por espacios (salvo los saltos
// de línea), recorta los espacios de los bordes y controla el largo.
func cleanText(s string, limit int) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("no es UTF-8 válido")
	}
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r != '\n' && unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
	if utf8.RuneCountInString(s) > limit {
		return "", fmt.Errorf("tiene más de %d caracteres", limit)
	}
	return s, nil
}

// Create guarda un pedido pendiente. Si el proyecto ya tiene uno esperando,
// devuelve *PendingExistsError con su ID.
func (s *Service) Create(ctx context.Context, r Request) (Approval, error) {
	reason, err := validate(r)
	if err != nil {
		return Approval{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Approval{}, fmt.Errorf("generar el ID: %w", err)
	}
	now := s.now().UTC()
	err = s.q.CreateApproval(ctx, store.CreateApprovalParams{
		ID: id, Operation: string(r.Operation), ServerID: r.ServerID, ProjectID: &r.ProjectID,
		ProjectName: r.ProjectName, Environment: r.Environment, CommitSha: r.Commit, Unit: r.Unit,
		Reason: reason, RequestedByToken: &r.TokenID, RequestedByName: r.TokenName,
		RequestedByPrefix: r.TokenPrefix, RequestedIp: truncate(r.IP, maxIPLen),
		RequestedAt: now, ExpiresAt: now.Add(TTL),
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		pending, getErr := s.q.PendingApprovalForProject(ctx, &r.ProjectID)
		if getErr != nil {
			// Se decidió entre el INSERT y esta consulta: igual había uno.
			return Approval{}, ErrPendingExists
		}
		return Approval{}, &PendingExistsError{ID: pending}
	}
	if err != nil {
		return Approval{}, fmt.Errorf("guardar el pedido: %w", err)
	}
	return s.Get(ctx, id)
}

// Get devuelve un pedido por su ID.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Approval, error) {
	row, err := s.q.GetApproval(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Approval{}, ErrNotFound
	}
	if err != nil {
		return Approval{}, fmt.Errorf("buscar el pedido: %w", err)
	}
	return fromRow(store.ListApprovalsRow(row)), nil
}

// List devuelve los pedidos, los más nuevos primero. Con status vacío, de
// todos los estados; con limit cero, DefaultListLimit.
func (s *Service) List(ctx context.Context, status Status, limit int) ([]Approval, error) {
	if status != "" && !ValidStatus(status) {
		return nil, fmt.Errorf("%w: estado desconocido %q", ErrInvalid, status)
	}
	if limit <= 0 {
		limit = DefaultListLimit
	}
	limit = min(limit, MaxListLimit)
	rows, err := s.q.ListApprovals(ctx, store.ListApprovalsParams{Status: string(status), MaxRows: int32(limit)}) //nolint:gosec // acotado arriba
	if err != nil {
		return nil, fmt.Errorf("listar los pedidos: %w", err)
	}
	out := make([]Approval, len(rows))
	for i, row := range rows {
		out[i] = fromRow(row)
	}
	return out, nil
}

// Approve aprueba un pedido pendiente que no venció. No ejecuta nada.
func (s *Service) Approve(ctx context.Context, id uuid.UUID, by Decider) (Approval, error) {
	return s.decide(ctx, id, StatusApproved, by, "")
}

// Reject rechaza un pedido pendiente, con un motivo opcional.
func (s *Service) Reject(ctx context.Context, id uuid.UUID, by Decider, note string) (Approval, error) {
	note, err := cleanText(note, MaxNoteLen)
	if err != nil {
		return Approval{}, fmt.Errorf("%w: el motivo %w", ErrInvalid, err)
	}
	return s.decide(ctx, id, StatusRejected, by, note)
}

func (s *Service) decide(ctx context.Context, id uuid.UUID, status Status, by Decider, note string) (Approval, error) {
	if n := utf8.RuneCountInString(by.Name); n < 1 || n > maxNameLen {
		return Approval{}, fmt.Errorf("%w: falta quién lo decide", ErrInvalid)
	}
	var userID *uuid.UUID
	if by.UserID != uuid.Nil {
		userID = &by.UserID
	}
	_, err := s.q.DecideApproval(ctx, store.DecideApprovalParams{
		Status: string(status), Now: s.now().UTC(), DecidedBy: userID, DecidedByName: by.Name,
		DecisionNote: note, ID: id,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return s.notPending(ctx, id)
	}
	if err != nil {
		return Approval{}, fmt.Errorf("decidir el pedido: %w", err)
	}
	return s.Get(ctx, id)
}

// Cancel cancela un pedido pendiente: lo decide el sistema, por ejemplo
// porque al ir a aprobarlo el token ya no servía.
func (s *Service) Cancel(ctx context.Context, id uuid.UUID, note string) (Approval, error) {
	note, err := cleanText(note, MaxNoteLen)
	if err != nil {
		return Approval{}, fmt.Errorf("%w: el motivo %w", ErrInvalid, err)
	}
	_, err = s.q.CancelApproval(ctx, store.CancelApprovalParams{Now: s.now().UTC(), Note: note, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return s.notPending(ctx, id)
	}
	if err != nil {
		return Approval{}, fmt.Errorf("cancelar el pedido: %w", err)
	}
	return s.Get(ctx, id)
}

// notPending dice por qué no se pudo decidir: no existe, o ya no estaba
// pendiente. Devuelve el pedido como está, para que se pueda mostrar.
func (s *Service) notPending(ctx context.Context, id uuid.UUID) (Approval, error) {
	a, err := s.Get(ctx, id)
	if err != nil {
		return Approval{}, err
	}
	return a, ErrNotPending
}

// SetDeploy anota el deploy que salió de un pedido aprobado.
func (s *Service) SetDeploy(ctx context.Context, id, deployID uuid.UUID) error {
	n, err := s.q.SetApprovalDeploy(ctx, store.SetApprovalDeployParams{DeployID: &deployID, ID: id})
	if err != nil {
		return fmt.Errorf("anotar el deploy del pedido: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("el pedido %s no está aprobado o ya tiene su deploy", id)
	}
	return nil
}

// Swept es lo que cambió un barrido.
type Swept struct {
	Expired   []Approval
	Cancelled []Approval
}

// Sweep vence los pedidos cuyo plazo pasó, y cancela los de un token
// revocado o vencido y los de un proyecto que dejó de estar declarado.
func (s *Service) Sweep(ctx context.Context) (Swept, error) {
	now := s.now().UTC()
	expired, err := s.q.ExpireApprovals(ctx, now)
	if err != nil {
		return Swept{}, fmt.Errorf("vencer los pedidos: %w", err)
	}
	deadTokens, err := s.q.CancelApprovalsOfDeadTokens(ctx, now)
	if err != nil {
		return Swept{}, fmt.Errorf("cancelar los pedidos de tokens que no sirven: %w", err)
	}
	orphans, err := s.q.CancelApprovalsWithoutProject(ctx, now)
	if err != nil {
		return Swept{}, fmt.Errorf("cancelar los pedidos sin proyecto: %w", err)
	}

	var out Swept
	if out.Expired, err = s.getAll(ctx, expired); err != nil {
		return Swept{}, err
	}
	if out.Cancelled, err = s.getAll(ctx, append(deadTokens, orphans...)); err != nil {
		return Swept{}, err
	}
	return out, nil
}

func (s *Service) getAll(ctx context.Context, ids []uuid.UUID) ([]Approval, error) {
	out := make([]Approval, 0, len(ids))
	for _, id := range ids {
		a, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func fromRow(row store.ListApprovalsRow) Approval {
	a := Approval{
		ID: row.ID, Operation: Operation(row.Operation), ServerID: row.ServerID, ServerName: row.ServerName,
		ProjectID: row.ProjectID, ProjectName: row.ProjectName, Environment: row.Environment,
		Commit: row.CommitSha, Unit: row.Unit, Reason: row.Reason,
		RequestedByToken: row.RequestedByToken, RequestedByName: row.RequestedByName,
		RequestedByPrefix: row.RequestedByPrefix, RequestedIP: row.RequestedIp,
		RequestedAt: row.RequestedAt, ExpiresAt: row.ExpiresAt, Status: Status(row.Status),
		DecidedAt: row.DecidedAt, DecisionNote: row.DecisionNote, DeployID: row.DeployID,
	}
	if row.DecidedByName != nil {
		a.DecidedByName = *row.DecidedByName
	}
	return a
}

func truncate(v string, limit int) string {
	if len(v) <= limit {
		return v
	}
	return v[:limit]
}
