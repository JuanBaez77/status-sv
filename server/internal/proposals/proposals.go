// Package proposals es el lado del control plane del alta de proyectos
// (ADR-022): arma la propuesta de un proyecto nuevo, la guarda y lleva la
// cuenta de qué pasó con ella en el agent.
//
// El control plane no declara proyectos. Lo que arma acá es una declaración
// completa que viaja al agent en project_proposal y espera a que una persona
// la acepte en el servidor. Lo que una propuesta no elige (el workdir, el
// usuario, el archivo de entorno) lo completa este paquete, y la declaración
// pasa por el mismo parser estricto que usa el agent.
package proposals

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JuanBaez77/status-sv/internal/projectspec"
	"github.com/JuanBaez77/status-sv/internal/templates"
	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// TTL es cuánto espera una propuesta a que la acepten (ADR-022).
const TTL = 7 * 24 * time.Hour

// maxList es cuántas propuestas devuelve la lista de un servidor.
const maxList = 50

// Estados de una propuesta.
const (
	StatusPending    = "pending"
	StatusDelivered  = "delivered"
	StatusAccepted   = "accepted"
	StatusRejected   = "rejected"
	StatusWithdrawn  = "withdrawn"
	StatusExpired    = "expired"
	StatusSuperseded = "superseded"
)

// Errores del servicio.
var (
	// ErrDeclared es que el servidor ya declara un proyecto con ese nombre.
	ErrDeclared = errors.New("ese servidor ya declara un proyecto con ese nombre")
	// ErrNotOpen es una propuesta que ya se cerró: aceptada, retirada o vencida.
	ErrNotOpen = errors.New("la propuesta ya no está abierta")
	// ErrInvalid es una propuesta que no es un project.yaml válido.
	ErrInvalid = errors.New("la propuesta no es válida")
)

// Request es una propuesta como la arma el panel (y, en el Sprint 8, el MCP):
// lo que se elige. Lo demás lo completa Build.
type Request struct {
	Name        string       `json:"name"`
	Environment string       `json:"environment"`
	Repo        string       `json:"repo"`
	Branch      string       `json:"branch"`
	Runtime     string       `json:"runtime"`
	ComposeFile string       `json:"compose_file,omitempty"`
	Command     []string     `json:"command,omitempty"`
	RequiredEnv []string     `json:"required_env,omitempty"`
	Site        *Site        `json:"site,omitempty"`
	Healthcheck *Healthcheck `json:"healthcheck,omitempty"`
	Build       []Step       `json:"build,omitempty"`
	Pre         []Step       `json:"pre,omitempty"`
	Post        []Step       `json:"post,omitempty"`
}

// Site es el sitio de nginx.
type Site struct {
	Domain string `json:"domain"`
	Port   int    `json:"port"`
}

// Healthcheck es el healthcheck declarado.
type Healthcheck struct {
	URL            string `json:"url"`
	ExpectStatus   int    `json:"expect_status,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// Step es un paso declarado.
type Step struct {
	Service        string   `json:"service,omitempty"`
	Command        []string `json:"command"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

// Build arma la declaración de una propuesta: completa lo que no se elige y
// la valida con el parser estricto, con lo que una propuesta no elige y con
// el tope de tamaño del contrato.
func Build(req Request) ([]byte, projectspec.Spec, error) {
	if !proto.ValidProjectName(req.Name) {
		return nil, projectspec.Spec{}, fmt.Errorf("%w: name: tiene que ser de minúsculas, números y guiones", ErrInvalid)
	}
	d := projectspec.Declaration{
		Name: req.Name, Environment: req.Environment, Repo: req.Repo, Branch: req.Branch, Runtime: req.Runtime,
		Workdir:     projectspec.ProposalWorkdir(req.Name),
		RequiredEnv: req.RequiredEnv,
	}
	if req.Runtime == string(proto.RuntimeDocker) {
		d.ComposeFile = req.ComposeFile
	} else {
		d.RunAs, d.EnvFile = req.Name, projectspec.ProposalEnvFile(req.Name)
		if len(req.Command) > 0 {
			d.Service = &projectspec.DeclaredService{Command: req.Command}
		}
	}
	if req.Site != nil {
		d.Site = &projectspec.DeclaredSite{Domain: req.Site.Domain, Port: req.Site.Port}
	}
	if req.Healthcheck != nil {
		d.Healthcheck = &projectspec.DeclaredHealthcheck{URL: req.Healthcheck.URL, ExpectStatus: req.Healthcheck.ExpectStatus, TimeoutSeconds: req.Healthcheck.TimeoutSeconds}
	}
	steps := func(in []Step) []projectspec.DeclaredStep {
		out := make([]projectspec.DeclaredStep, 0, len(in))
		for _, s := range in {
			out = append(out, projectspec.DeclaredStep{Service: s.Service, Command: s.Command, TimeoutSeconds: s.TimeoutSeconds})
		}
		return out
	}
	if len(req.Build)+len(req.Pre)+len(req.Post) > 0 {
		d.Deploy = &projectspec.DeclaredSteps{Build: steps(req.Build), Pre: steps(req.Pre), Post: steps(req.Post)}
	}

	data, spec, err := projectspec.Encode(d)
	if err != nil {
		return nil, projectspec.Spec{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := projectspec.CheckProposal(spec); err != nil {
		return nil, projectspec.Spec{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if len(data) > proto.MaxProposalBytes {
		return nil, projectspec.Spec{}, fmt.Errorf("%w: la declaración pesa %d bytes y el máximo es %d", ErrInvalid, len(data), proto.MaxProposalBytes)
	}
	return data, spec, nil
}

// Preview es lo que se va a escribir en el servidor, con las plantillas de
// fábrica (ADR-023): el que sabe si el servidor tiene las suyas es el comando
// de aceptación, que muestra lo que escribe de verdad.
type Preview struct {
	YAML    string `json:"yaml"`
	Unit    string `json:"unit,omitempty"`
	Site    string `json:"site,omitempty"`
	Command string `json:"command"`
}

// MakePreview arma la declaración y lo que generan las plantillas.
func MakePreview(req Request) (Preview, error) {
	data, spec, err := Build(req)
	if err != nil {
		return Preview{}, err
	}
	return previewOf(data, spec)
}

func previewOf(data []byte, spec projectspec.Spec) (Preview, error) {
	p := Preview{YAML: string(data), Command: AcceptCommand(spec.Project.Name)}
	set := templates.Default()
	if spec.Service != nil {
		unit, err := set.Service(spec)
		if err != nil {
			return Preview{}, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		p.Unit = string(unit)
	}
	if spec.Site != nil {
		site, err := set.Site(spec, templates.SiteTLS)
		if err != nil {
			return Preview{}, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		p.Site = string(site)
	}
	return p, nil
}

// AcceptCommand es lo que corre una persona en el servidor para aceptarla.
func AcceptCommand(name string) string {
	return "sudo statussv-agent project accept " + name
}

// Actor es quién propone: una persona desde el panel o, más adelante, un LLM
// por el MCP.
type Actor struct {
	UserID *uuid.UUID
	Name   string
	Type   string // "user" o "llm"
}

// Service guarda las propuestas.
type Service struct {
	pool *pgxpool.Pool
	q    *store.Queries
	now  func() time.Time
}

// NewService arma el servicio.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: store.New(pool), now: time.Now}
}

// Create arma y guarda una propuesta, y cierra la abierta con el mismo nombre
// en ese servidor, si había: la reemplaza.
func (s *Service) Create(ctx context.Context, serverID uuid.UUID, req Request, actor Actor) (store.ProjectProposal, error) {
	data, spec, err := Build(req)
	if err != nil {
		return store.ProjectProposal{}, err
	}
	declared, err := s.q.ProjectDeclaredOnServer(ctx, store.ProjectDeclaredOnServerParams{ServerID: serverID, Name: req.Name})
	if err != nil {
		return store.ProjectProposal{}, err
	}
	if declared {
		return store.ProjectProposal{}, ErrDeclared
	}
	id, err := uuid.NewV7()
	if err != nil {
		return store.ProjectProposal{}, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	var created store.ProjectProposal
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.SupersedeProposal(ctx, store.SupersedeProposalParams{ServerID: serverID, Name: req.Name, At: now}); err != nil {
			return err
		}
		created, err = q.CreateProposal(ctx, store.CreateProposalParams{
			ID: id, ServerID: serverID, Name: req.Name, Runtime: string(spec.Project.Runtime),
			Environment: string(spec.Project.Environment), Spec: string(data), SpecHash: proto.SpecHash(data),
			CreatedBy: actor.UserID, CreatedByName: actor.Name, ActorType: actor.Type,
			CreatedAt: now, ExpiresAt: now.Add(TTL),
		})
		return err
	})
	return created, err
}

// Get devuelve una propuesta.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (store.GetProposalRow, error) {
	return s.q.GetProposal(ctx, id)
}

// List devuelve las propuestas de un servidor.
func (s *Service) List(ctx context.Context, serverID uuid.UUID) ([]store.ProjectProposal, error) {
	return s.q.ListServerProposals(ctx, store.ListServerProposalsParams{ServerID: serverID, MaxRows: maxList})
}

// PreviewOf arma la vista previa de una propuesta guardada.
func PreviewOf(p store.ProjectProposal) (Preview, error) {
	spec, err := projectspec.ParseSpec(p.Name+".yaml", []byte(p.Spec))
	if err != nil {
		return Preview{}, err
	}
	return previewOf([]byte(p.Spec), spec)
}

// Withdraw retira una propuesta abierta.
func (s *Service) Withdraw(ctx context.Context, id uuid.UUID) (store.ProjectProposal, error) {
	return s.close(ctx, id, StatusWithdrawn, "")
}

// Rejected cierra una propuesta que el agent no guardó.
func (s *Service) Rejected(ctx context.Context, id uuid.UUID, message string) (store.ProjectProposal, error) {
	return s.close(ctx, id, StatusRejected, message)
}

func (s *Service) close(ctx context.Context, id uuid.UUID, status, message string) (store.ProjectProposal, error) {
	p, err := s.q.CloseProposal(ctx, store.CloseProposalParams{ID: id, Status: status, Message: message, At: s.now().UTC()})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ProjectProposal{}, ErrNotOpen
	}
	return p, err
}

// Delivered anota que el agent guardó la propuesta. Dice si cambió algo.
func (s *Service) Delivered(ctx context.Context, serverID, id uuid.UUID) (bool, error) {
	n, err := s.q.MarkProposalDelivered(ctx, store.MarkProposalDeliveredParams{ID: id, ServerID: serverID, At: s.now().UTC()})
	return n > 0, err
}

// Open devuelve las propuestas abiertas de un servidor.
func (s *Service) Open(ctx context.Context, serverID uuid.UUID) ([]store.ProjectProposal, error) {
	return s.q.OpenProposals(ctx, store.OpenProposalsParams{ServerID: serverID, Now: s.now().UTC()})
}

// Expire cierra las propuestas vencidas.
func (s *Service) Expire(ctx context.Context) ([]store.ProjectProposal, error) {
	return s.q.ExpireProposals(ctx, s.now().UTC())
}

// Accepted es una propuesta que se aceptó en el servidor: el agent informa un
// proyecto declarado con su nombre. Same dice si lo declarado es exactamente
// lo propuesto.
type Accepted struct {
	Proposal store.ProjectProposal
	Same     bool
}

// Reconciliation es lo que hay que hacer después de comparar lo que tiene el
// agent con lo que dice la base.
type Reconciliation struct {
	// Accepted son las propuestas que se declararon.
	Accepted []Accepted
	// Delivered son las que el agent tiene y se anotaron como entregadas.
	Delivered int
	// Send son las abiertas que el agent no tiene: hay que mandárselas.
	Send []store.ProjectProposal
	// Withdraw son las que el agent tiene y ya no están abiertas: hay que
	// pedirle que las borre.
	Withdraw []proto.PendingProposal
}

// Reconcile compara las propuestas que informa el agent en projects_state con
// las abiertas de la base. Si la lectura de proyectos fue buena, una
// propuesta cuyo nombre aparece declarado queda aceptada.
func (s *Service) Reconcile(ctx context.Context, serverID uuid.UUID, state proto.ProjectsState) (Reconciliation, error) {
	var r Reconciliation
	open, err := s.Open(ctx, serverID)
	if err != nil {
		return r, err
	}
	declared := make(map[string]proto.Project, len(state.Projects))
	if state.Source.Status == proto.SourceOK {
		for _, p := range state.Projects {
			declared[p.Name] = p
		}
	}
	reported := make(map[uuid.UUID]bool, len(state.Proposals))
	for _, p := range state.Proposals {
		reported[p.ProposalID] = true
	}

	for _, p := range open {
		if project, ok := declared[p.Name]; ok {
			same := project.SpecHash == p.SpecHash
			message := "declarada igual a la propuesta"
			if !same {
				message = "declarada distinta de la propuesta"
			}
			closed, err := s.close(ctx, p.ID, StatusAccepted, message)
			if errors.Is(err, ErrNotOpen) {
				continue
			}
			if err != nil {
				return r, err
			}
			r.Accepted = append(r.Accepted, Accepted{Proposal: closed, Same: same})
			continue
		}
		if reported[p.ID] {
			changed, err := s.Delivered(ctx, serverID, p.ID)
			if err != nil {
				return r, err
			}
			if changed {
				r.Delivered++
			}
			continue
		}
		r.Send = append(r.Send, p)
	}

	if len(state.Proposals) > 0 {
		ids := make([]uuid.UUID, 0, len(state.Proposals))
		for _, p := range state.Proposals {
			ids = append(ids, p.ProposalID)
		}
		known, err := s.q.KnownProposals(ctx, store.KnownProposalsParams{ServerID: serverID, Ids: ids, Now: s.now().UTC()})
		if err != nil {
			return r, err
		}
		stillOpen := make(map[uuid.UUID]bool, len(known))
		for _, id := range known {
			stillOpen[id] = true
		}
		for _, p := range state.Proposals {
			if !stillOpen[p.ProposalID] {
				r.Withdraw = append(r.Withdraw, p)
			}
		}
	}
	return r, nil
}

// Message arma el mensaje para el agent de una propuesta abierta.
func Message(p store.ProjectProposal) proto.ProjectProposal {
	return proto.ProjectProposal{ProposalID: p.ID, Name: p.Name, Action: proto.ProposalPut, YAML: p.Spec, ExpiresAt: p.ExpiresAt.UTC()}
}

// WithdrawMessage arma el pedido de borrar una propuesta.
func WithdrawMessage(id uuid.UUID, name string) proto.ProjectProposal {
	return proto.ProjectProposal{ProposalID: id, Name: name, Action: proto.ProposalWithdraw}
}
