package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/JuanBaez77/status-sv/server/internal/runstate"
)

// panelRuntime es el mensaje del WebSocket del panel con una foto de runtime
// recién guardada.
const panelRuntime = "runtime"

// runtimeResponse es el estado de runtime de un servidor, como lo ve el panel:
// lo mismo por la API REST y por el WebSocket, así el front usa un solo código.
type runtimeResponse struct {
	ServerID uuid.UUID `json:"server_id"`
	// TakenAt es la marca de la última foto; falta si el agent todavía no
	// mandó ninguna (un agent viejo, por ejemplo).
	TakenAt    *time.Time               `json:"taken_at"`
	Sources    map[string]runtimeSource `json:"sources"`
	Containers []runtimeContainer       `json:"containers"`
	Services   []runtimeService         `json:"services"`
	Processes  []runtimeProcess         `json:"processes"`
}

type runtimeSource struct {
	Status  string    `json:"status"`
	Detail  string    `json:"detail,omitempty"`
	Omitted int32     `json:"omitted"`
	TakenAt time.Time `json:"taken_at"`
}

// runtimeUsage falta (null) cuando no hay medición: un contenedor detenido, o
// la primera foto después de que el agent arrancó. No es lo mismo que cero.
type runtimeUsage struct {
	CPUPercent  float32 `json:"cpu_percent"`
	MemoryBytes int64   `json:"memory_bytes"`
}

type runtimeContainer struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Image          string        `json:"image"`
	State          string        `json:"state"`
	Health         string        `json:"health"`
	StartedAt      *time.Time    `json:"started_at"`
	ComposeProject string        `json:"compose_project"`
	ComposeService string        `json:"compose_service"`
	Usage          *runtimeUsage `json:"usage"`
	SeenAt         time.Time     `json:"seen_at"`
}

type runtimeService struct {
	Unit        string        `json:"unit"`
	LoadState   string        `json:"load_state"`
	ActiveState string        `json:"active_state"`
	SubState    string        `json:"sub_state"`
	MainPID     int32         `json:"main_pid"`
	ActiveSince *time.Time    `json:"active_since"`
	Usage       *runtimeUsage `json:"usage"`
	SeenAt      time.Time     `json:"seen_at"`
}

type runtimeProcess struct {
	PID       int32         `json:"pid"`
	Name      string        `json:"name"`
	Command   string        `json:"command"`
	User      string        `json:"user"`
	StartedAt *time.Time    `json:"started_at"`
	Usage     *runtimeUsage `json:"usage"`
	SeenAt    time.Time     `json:"seen_at"`
}

// panelRuntimeMessage lleva la foto por el WebSocket del panel.
type panelRuntimeMessage struct {
	Type     string          `json:"type"`
	ServerID uuid.UUID       `json:"server_id"`
	Runtime  runtimeResponse `json:"runtime"`
}

// handleServerRuntime devuelve el último estado de runtime conocido de un
// servidor: GET /v1/servers/{id}/runtime.
func (s *Server) handleServerRuntime(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)

	serverID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el ID del servidor no es un UUID"})
		return
	}

	runtime, err := s.readRuntime(ctx, serverID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeJSON(ctx, w, http.StatusNotFound, errorResponse{Error: "not_found", Message: "no existe ese servidor"})
		return
	case err != nil:
		s.log.ErrorContext(ctx, "no se pudo leer el estado de runtime", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	writeJSON(ctx, w, http.StatusOK, runtime)
}

// readRuntime arma el estado de runtime de un servidor desde la base. Si el
// servidor no existe devuelve pgx.ErrNoRows.
func (s *Server) readRuntime(ctx context.Context, serverID uuid.UUID) (runtimeResponse, error) {
	server, err := s.queries.GetServerByID(ctx, serverID)
	if err != nil {
		return runtimeResponse{}, err
	}
	sources, err := s.queries.ListRuntimeSources(ctx, serverID)
	if err != nil {
		return runtimeResponse{}, fmt.Errorf("listar las fuentes: %w", err)
	}
	units, err := s.queries.ListRuntimeUnits(ctx, serverID)
	if err != nil {
		return runtimeResponse{}, fmt.Errorf("listar las unidades: %w", err)
	}

	out := runtimeResponse{
		ServerID:   serverID,
		TakenAt:    server.RuntimeTakenAt,
		Sources:    make(map[string]runtimeSource, len(sources)),
		Containers: []runtimeContainer{},
		Services:   []runtimeService{},
		Processes:  []runtimeProcess{},
	}
	for _, src := range sources {
		out.Sources[src.Source] = runtimeSource{
			Status: src.Status, Detail: src.Detail, Omitted: src.Omitted, TakenAt: src.TakenAt.UTC(),
		}
	}
	for _, u := range units {
		usage := usageOf(u.CpuPercent, u.MemoryBytes)
		started := utcPtr(u.StartedAt)
		switch u.Kind {
		case runstate.KindContainer:
			out.Containers = append(out.Containers, runtimeContainer{
				ID: u.UnitKey, Name: u.Name, Image: u.Image, State: u.State, Health: u.Health,
				StartedAt: started, ComposeProject: u.ComposeProject, ComposeService: u.ComposeService,
				Usage: usage, SeenAt: u.SeenAt.UTC(),
			})
		case runstate.KindService:
			out.Services = append(out.Services, runtimeService{
				Unit: u.Name, LoadState: u.LoadState, ActiveState: u.State, SubState: u.SubState,
				MainPID: u.Pid, ActiveSince: started, Usage: usage, SeenAt: u.SeenAt.UTC(),
			})
		case runstate.KindProcess:
			out.Processes = append(out.Processes, runtimeProcess{
				PID: u.Pid, Name: u.Name, Command: u.Command, User: u.UserName,
				StartedAt: started, Usage: usage, SeenAt: u.SeenAt.UTC(),
			})
		}
	}
	return out, nil
}

// publishRuntime manda el estado de runtime recién guardado a los paneles que
// miran ese servidor. Lo lee de la base y no de la foto: con una fuente en
// error, lo que se muestra es lo que quedó guardado, no la foto vacía.
func (s *Server) publishRuntime(ctx context.Context, serverID uuid.UUID) {
	if !s.panels.hasSubscribers(serverID) {
		return
	}
	runtime, err := s.readRuntime(ctx, serverID)
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo leer el estado de runtime para el panel", "error", err)
		return
	}
	raw, err := json.Marshal(panelRuntimeMessage{Type: panelRuntime, ServerID: serverID, Runtime: runtime})
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo armar el estado de runtime para el panel", "error", err)
		return
	}
	s.panels.publish(serverID, raw)
}

func usageOf(cpu pgtype.Float4, memory pgtype.Int8) *runtimeUsage {
	if !cpu.Valid || !memory.Valid {
		return nil
	}
	return &runtimeUsage{CPUPercent: cpu.Float32, MemoryBytes: memory.Int64}
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
