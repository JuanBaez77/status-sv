package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JuanBaez77/status-sv/server/internal/projects"
	"github.com/JuanBaez77/status-sv/server/internal/runstate"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// panelProjects es el aviso del WebSocket del panel de que cambiaron los
// proyectos declarados de un servidor. No trae los proyectos: el panel vuelve a
// pedir lo que está mirando.
const panelProjects = "projects"

type panelProjectsMessage struct {
	Type     string    `json:"type"`
	ServerID uuid.UUID `json:"server_id"`
}

// projectSummary es un proyecto como lo muestra la lista.
type projectSummary struct {
	ID          uuid.UUID       `json:"id"`
	Name        string          `json:"name"`
	ServerID    uuid.UUID       `json:"server_id"`
	ServerName  string          `json:"server_name"`
	Environment string          `json:"environment"`
	Runtime     string          `json:"runtime"`
	Repo        string          `json:"repo"`
	Branch      string          `json:"branch"`
	Status      projects.Status `json:"status"`
	// Units cuenta lo que tiene que correr y cuánto de eso está bien.
	Units  unitCounts     `json:"units"`
	Health *projectHealth `json:"health"`
	SeenAt time.Time      `json:"seen_at"`
}

type unitCounts struct {
	Total   int `json:"total"`
	Healthy int `json:"healthy"`
}

type projectHealth struct {
	URL            string     `json:"url"`
	ExpectStatus   int32      `json:"expect_status"`
	TimeoutSeconds int32      `json:"timeout_seconds"`
	State          string     `json:"state,omitempty"`
	CheckedAt      *time.Time `json:"checked_at,omitempty"`
	StatusCode     int32      `json:"status_code,omitempty"`
	LatencyMS      int32      `json:"latency_ms,omitempty"`
	Detail         string     `json:"detail,omitempty"`
}

// projectDetail es un proyecto con lo que se ve de él en el servidor.
type projectDetail struct {
	projectSummary
	Workdir        string   `json:"workdir"`
	ComposeFile    string   `json:"compose_file,omitempty"`
	ComposeProject string   `json:"compose_project,omitempty"`
	DeclaredUnits  []string `json:"declared_units,omitempty"`
	// MissingUnits son los servicios declarados que el agent no ve.
	MissingUnits []string           `json:"missing_units"`
	Containers   []runtimeContainer `json:"containers"`
	Services     []runtimeService   `json:"services"`
	// Source dice si el agent pudo leer Docker o systemd, según el runtime.
	Source *runtimeSource `json:"source"`
}

type projectListResponse struct {
	Projects []projectSummary `json:"projects"`
	// Invalid son los archivos de proyecto que no se pudieron usar.
	Invalid []invalidProjectFile `json:"invalid"`
	// ReadErrors son los servidores cuya última lectura de proyectos falló: lo
	// que se muestra de ellos puede estar viejo.
	ReadErrors []projectReadError `json:"read_errors"`
}

type invalidProjectFile struct {
	ServerID   uuid.UUID `json:"server_id"`
	ServerName string    `json:"server_name"`
	File       string    `json:"file"`
	Error      string    `json:"error"`
	SeenAt     time.Time `json:"seen_at"`
}

type projectReadError struct {
	ServerID   uuid.UUID  `json:"server_id"`
	ServerName string     `json:"server_name"`
	Detail     string     `json:"detail"`
	TakenAt    *time.Time `json:"taken_at"`
}

// handleListProjects devuelve todos los proyectos declarados, con su estado:
// GET /v1/projects.
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)

	out, err := s.readProjects(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudieron leer los proyectos", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	writeJSON(ctx, w, http.StatusOK, out)
}

// handleGetProject devuelve un proyecto con sus contenedores o servicios:
// GET /v1/projects/{id}.
func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el ID del proyecto no es un UUID"})
		return
	}
	row, err := s.queries.GetProject(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(ctx, w, http.StatusNotFound, errorResponse{Error: "not_found", Message: "no existe ese proyecto"})
		return
	}
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo leer el proyecto", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	runtime, err := s.readRuntime(ctx, row.ServerID)
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo leer el estado de runtime del proyecto", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	writeJSON(ctx, w, http.StatusOK, buildDetail(store.ListProjectsRow(row), runtime))
}

// readProjects arma la lista de proyectos. El estado de runtime se lee una vez
// por servidor.
func (s *Server) readProjects(ctx context.Context) (projectListResponse, error) {
	rows, err := s.queries.ListProjects(ctx)
	if err != nil {
		return projectListResponse{}, fmt.Errorf("listar los proyectos: %w", err)
	}
	out := projectListResponse{Projects: []projectSummary{}, Invalid: []invalidProjectFile{}, ReadErrors: []projectReadError{}}

	runtimes := make(map[uuid.UUID]runtimeResponse)
	for _, row := range rows {
		runtime, ok := runtimes[row.ServerID]
		if !ok {
			if runtime, err = s.readRuntime(ctx, row.ServerID); err != nil {
				return projectListResponse{}, fmt.Errorf("leer el estado de runtime de %s: %w", row.ServerName, err)
			}
			runtimes[row.ServerID] = runtime
		}
		out.Projects = append(out.Projects, buildDetail(row, runtime).projectSummary)
	}

	invalid, err := s.queries.ListInvalidProjectFiles(ctx)
	if err != nil {
		return projectListResponse{}, fmt.Errorf("listar los archivos inválidos: %w", err)
	}
	for _, f := range invalid {
		out.Invalid = append(out.Invalid, invalidProjectFile{
			ServerID: f.ServerID, ServerName: f.ServerName, File: f.File, Error: f.Error, SeenAt: f.SeenAt.UTC(),
		})
	}
	readErrors, err := s.queries.ListProjectReadErrors(ctx)
	if err != nil {
		return projectListResponse{}, fmt.Errorf("listar las lecturas fallidas: %w", err)
	}
	for _, e := range readErrors {
		out.ReadErrors = append(out.ReadErrors, projectReadError{
			ServerID: e.ID, ServerName: e.Name, Detail: e.ProjectsDetail, TakenAt: utcPtr(e.ProjectsTakenAt),
		})
	}
	return out, nil
}

// buildDetail cruza un proyecto con el estado de runtime de su servidor: los
// contenedores por la etiqueta de Compose y los servicios por la lista
// declarada (ADR-012).
func buildDetail(row store.ListProjectsRow, runtime runtimeResponse) projectDetail {
	d := projectDetail{
		projectSummary: projectSummary{
			ID: row.ID, Name: row.Name, ServerID: row.ServerID, ServerName: row.ServerName,
			Environment: row.Environment, Runtime: row.Runtime, Repo: row.Repo, Branch: row.Branch,
			SeenAt: row.SeenAt.UTC(),
		},
		Workdir:        row.Workdir,
		ComposeFile:    row.ComposeFile,
		ComposeProject: row.ComposeProject,
		DeclaredUnits:  row.Units,
		MissingUnits:   []string{},
		Containers:     []runtimeContainer{},
		Services:       []runtimeService{},
	}
	if row.HealthcheckUrl != "" {
		d.Health = &projectHealth{
			URL: row.HealthcheckUrl, ExpectStatus: row.HealthcheckExpectStatus, TimeoutSeconds: row.HealthcheckTimeoutSeconds,
			State: row.HealthState, CheckedAt: utcPtr(row.HealthCheckedAt), StatusCode: row.HealthStatusCode,
			LatencyMS: row.HealthLatencyMs, Detail: row.HealthDetail,
		}
	}

	sourceName := runstate.SourceDocker
	if row.Runtime == "systemd" {
		sourceName = runstate.SourceSystemd
	}
	if src, ok := runtime.Sources[sourceName]; ok {
		d.Source = &src
	}

	switch row.Runtime {
	case "docker":
		for _, c := range runtime.Containers {
			if c.ComposeProject != row.ComposeProject {
				continue
			}
			d.Containers = append(d.Containers, c)
			if counts, healthy := (projects.Container{State: c.State, Health: c.Health}).Counts(); counts {
				d.Units.Total++
				if healthy {
					d.Units.Healthy++
				}
			}
		}
	case "systemd":
		for _, unit := range row.Units {
			d.Units.Total++
			i := slices.IndexFunc(runtime.Services, func(s runtimeService) bool { return s.Unit == unit })
			if i < 0 {
				d.MissingUnits = append(d.MissingUnits, unit)
				continue
			}
			d.Services = append(d.Services, runtime.Services[i])
			if projects.ServiceHealthy(runtime.Services[i].ActiveState) {
				d.Units.Healthy++
			}
		}
	}

	sourceOK := d.Source != nil && d.Source.Status == "ok"
	d.Status = projects.Derive(sourceOK, d.Units.Total, d.Units.Healthy, row.HealthState)
	return d
}

// publishProjects avisa a los paneles que miran un servidor que cambiaron sus
// proyectos declarados.
func (s *Server) publishProjects(ctx context.Context, serverID uuid.UUID) {
	if !s.panels.hasSubscribers(serverID) {
		return
	}
	raw, err := json.Marshal(panelProjectsMessage{Type: panelProjects, ServerID: serverID})
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo armar el aviso de proyectos", "error", err)
		return
	}
	s.panels.publish(serverID, raw)
}
