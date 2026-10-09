package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// Límites de la lectura (ADR-020).
const (
	// DefaultPage es cuántos renglones se devuelven si no se pide otra cosa.
	DefaultPage = 50
	// MaxPage es lo más que se devuelve de una vez.
	MaxPage = 100
)

// ErrInvalidFilter es un filtro o un cursor que no tiene el formato esperado.
var ErrInvalidFilter = errors.New("filtro de auditoría inválido")

// Filter acota la lectura. Los campos vacíos no filtran.
type Filter struct {
	ActorType string
	// Actor es quién actuó, tal cual: el usuario, el nombre del token del MCP.
	Actor      string
	Action     string
	TargetType string
	TargetID   string
	Result     string
	// From y To acotan el instante: desde From inclusive hasta To exclusive.
	From, To time.Time
	// Before es el cursor que devolvió la página anterior.
	Before string
	// Limit es el tamaño de la página; cero es DefaultPage.
	Limit int
}

// Row es un renglón de auditoría tal como se lee.
type Row struct {
	ID         uuid.UUID       `json:"id"`
	At         time.Time       `json:"at"`
	Actor      string          `json:"actor"`
	ActorType  string          `json:"actor_type"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	Result     string          `json:"result"`
	IP         string          `json:"ip,omitempty"`
	Detail     json.RawMessage `json:"detail"`
}

// Page es una página de renglones. Next es el cursor de la siguiente, o vacío
// si no hay más.
type Page struct {
	Rows []Row
	Next string
}

// lister es lo que se usa de la base para leer.
type lister interface {
	ListAudit(ctx context.Context, arg store.ListAuditParams) ([]store.AuditLog, error)
}

// Reader lee la auditoría.
type Reader struct {
	db lister
}

// NewReader arma un Reader sobre la base.
func NewReader(db lister) *Reader {
	return &Reader{db: db}
}

// Los filtros de texto se comparan contra columnas que solo escribe este
// paquete, así que alcanza con validar el formato: una acción es "algo.algo"
// y el resto, palabras en minúscula.
var (
	actionFormat = regexp.MustCompile(`^[a-z_]+\.[a-z_]+$`)
	wordFormat   = regexp.MustCompile(`^[a-z_]+$`)
)

// Validate revisa el formato de cada filtro.
func (f Filter) Validate() error {
	switch ActorType(f.ActorType) {
	case "", ActorUser, ActorLLM, ActorAgent, ActorSystem:
	default:
		return fmt.Errorf("%w: tipo de actor desconocido", ErrInvalidFilter)
	}
	if f.Action != "" && (len(f.Action) > maxAction || !actionFormat.MatchString(f.Action)) {
		return fmt.Errorf("%w: la acción tiene la forma grupo.accion", ErrInvalidFilter)
	}
	if f.TargetType != "" && (len(f.TargetType) > maxTargetType || !wordFormat.MatchString(f.TargetType)) {
		return fmt.Errorf("%w: tipo de objetivo inválido", ErrInvalidFilter)
	}
	if f.Result != "" && (len(f.Result) > maxResult || !wordFormat.MatchString(f.Result)) {
		return fmt.Errorf("%w: resultado inválido", ErrInvalidFilter)
	}
	if len(f.Actor) > maxActor || strings.ContainsFunc(f.Actor, unicode.IsControl) {
		return fmt.Errorf("%w: actor inválido", ErrInvalidFilter)
	}
	if len(f.TargetID) > maxTargetID || strings.ContainsFunc(f.TargetID, unicode.IsControl) {
		return fmt.Errorf("%w: objetivo inválido", ErrInvalidFilter)
	}
	if !f.From.IsZero() && !f.To.IsZero() && !f.From.Before(f.To) {
		return fmt.Errorf("%w: el rango de fechas está al revés", ErrInvalidFilter)
	}
	if f.Limit < 0 || f.Limit > MaxPage {
		return fmt.Errorf("%w: el tamaño de página va de 1 a %d", ErrInvalidFilter, MaxPage)
	}
	return nil
}

// List devuelve una página, del renglón más nuevo al más viejo.
func (r *Reader) List(ctx context.Context, f Filter) (Page, error) {
	if err := f.Validate(); err != nil {
		return Page{}, err
	}
	limit := f.Limit
	if limit == 0 {
		limit = DefaultPage
	}

	// Se pide uno de más para saber si hay otra página sin contar la tabla.
	params := store.ListAuditParams{
		ActorType:  optional(f.ActorType),
		Actor:      optional(f.Actor),
		Action:     optional(f.Action),
		TargetType: optional(f.TargetType),
		TargetID:   optional(f.TargetID),
		Result:     optional(f.Result),
		MaxRows:    int32(limit) + 1, //nolint:gosec // limit ya está acotado por MaxPage
	}
	if !f.From.IsZero() {
		params.FromAt = &f.From
	}
	if !f.To.IsZero() {
		params.ToAt = &f.To
	}
	if f.Before != "" {
		at, id, err := parseCursor(f.Before)
		if err != nil {
			return Page{}, err
		}
		params.BeforeAt, params.BeforeID = &at, &id
	}

	found, err := r.db.ListAudit(ctx, params)
	if err != nil {
		return Page{}, fmt.Errorf("leer la auditoría: %w", err)
	}

	page := Page{Rows: make([]Row, 0, min(len(found), limit))}
	for i, a := range found {
		if i == limit {
			last := found[limit-1]
			page.Next = cursor(last.At, last.ID)
			break
		}
		page.Rows = append(page.Rows, Row{
			ID: a.ID, At: a.At, Actor: a.Actor, ActorType: a.ActorType, Action: a.Action,
			TargetType: a.TargetType, TargetID: a.TargetID, Result: a.Result, IP: a.Ip, Detail: a.Detail,
		})
	}
	return page, nil
}

// cursor arma el cursor de un renglón: su instante en microsegundos, que es
// la precisión de la base, y su ID.
func cursor(at time.Time, id uuid.UUID) string {
	return strconv.FormatInt(at.UnixMicro(), 10) + "_" + id.String()
}

func parseCursor(s string) (time.Time, uuid.UUID, error) {
	micros, rawID, ok := strings.Cut(s, "_")
	if !ok {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: cursor inválido", ErrInvalidFilter)
	}
	n, err := strconv.ParseInt(micros, 10, 64)
	if err != nil || n < 0 {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: cursor inválido", ErrInvalidFilter)
	}
	id, err := uuid.Parse(rawID)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: cursor inválido", ErrInvalidFilter)
	}
	return time.UnixMicro(n).UTC(), id, nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
