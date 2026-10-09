package mcp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JuanBaez77/status-sv/server/internal/tokens"
)

// versionSearch es cuántos deploys se miran para saber qué versión corre.
const versionSearch = 20

func (s *Server) addProjectTools() {
	addTool(s, &sdk.Tool{
		Name: "list_projects",
		Description: "Lista los proyectos que alcanza este token, con su servidor, entorno, runtime " +
			"(docker o systemd), rama y estado (ok, degraded, down o unknown). Se puede filtrar por " +
			"servidor y por entorno.",
		Annotations: readOnly("Proyectos"),
	}, classRead, s.listProjects)

	addTool(s, &sdk.Tool{
		Name: "get_project_status",
		Description: "El estado de un proyecto: sus contenedores o servicios, el healthcheck, la versión " +
			"que corre (el commit del último deploy que quedó andando), el último deploy y los nombres " +
			"de las variables requeridas que faltan en el servidor. El proyecto se nombra con " +
			"servidor/nombre, con su nombre si no hay otro igual, o con su ID.",
		Annotations: readOnly("Estado de un proyecto"),
	}, classRead, s.projectStatus)
}

type listProjectsInput struct {
	Server      string `json:"server,omitempty" jsonschema:"solo los de este servidor (su nombre)"`
	Environment string `json:"environment,omitempty" jsonschema:"solo los de este entorno: staging o production"`
}

type listProjectsOutput struct {
	Projects []projectOutput `json:"projects"`
}

type projectOutput struct {
	ID          string        `json:"id"`
	Ref         string        `json:"ref" jsonschema:"servidor/nombre: cómo nombrarlo en las demás herramientas"`
	Server      string        `json:"server"`
	Name        string        `json:"name"`
	Environment string        `json:"environment"`
	Runtime     string        `json:"runtime"`
	Repo        string        `json:"repo"`
	Branch      string        `json:"branch"`
	Status      string        `json:"status" jsonschema:"ok: todo bien; degraded: algo anda mal; down: no corre o el healthcheck falla; unknown: el agent no pudo mirar"`
	Units       unitsOutput   `json:"units"`
	Healthcheck *healthOutput `json:"healthcheck,omitempty"`
	SeenAt      string        `json:"seen_at" jsonschema:"cuándo informó el agent la declaración por última vez"`
}

type unitsOutput struct {
	Total   int `json:"total"`
	Healthy int `json:"healthy"`
}

type healthOutput struct {
	URL        string `json:"url"`
	State      string `json:"state,omitempty" jsonschema:"ok o failing; vacío si todavía no se chequeó"`
	StatusCode int    `json:"status_code,omitempty"`
	LatencyMS  int    `json:"latency_ms,omitempty"`
	CheckedAt  string `json:"checked_at,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

func (s *Server) listProjects(ctx context.Context, call *Call, in listProjectsInput) (listProjectsOutput, error) {
	if in.Environment != "" && in.Environment != tokens.EnvStaging && in.Environment != tokens.EnvProduction {
		return listProjectsOutput{}, errInvalid("el entorno tiene que ser staging o production")
	}
	if !call.Token.CanRead() {
		return listProjectsOutput{}, errDenied("este token no tiene permiso de lectura")
	}
	all, err := s.backend.Projects(ctx)
	if err != nil {
		return listProjectsOutput{}, err
	}
	out := listProjectsOutput{Projects: []projectOutput{}}
	for _, p := range visibleProjects(call.Token, tokens.CapRead, all) {
		if (in.Server != "" && p.ServerName != in.Server) || (in.Environment != "" && p.Environment != in.Environment) {
			continue
		}
		out.Projects = append(out.Projects, projectFrom(p))
	}
	call.Note("proyectos", len(out.Projects))
	return out, nil
}

type projectStatusInput struct {
	Project string `json:"project" jsonschema:"servidor/nombre, el nombre solo si no hay otro igual, o el ID"`
}

type projectStatusOutput struct {
	Project       projectOutput     `json:"project"`
	Workdir       string            `json:"workdir"`
	ComposeFile   string            `json:"compose_file,omitempty"`
	ComposeName   string            `json:"compose_project,omitempty"`
	Containers    []containerOutput `json:"containers"`
	Services      []serviceOutput   `json:"services"`
	MissingUnits  []string          `json:"missing_units" jsonschema:"servicios declarados que el agent no ve en el servidor"`
	RuntimeSource string            `json:"runtime_source" jsonschema:"ok si el agent pudo leer Docker o systemd; error si no; unknown si nunca informó"`
	Site          *siteOutput       `json:"site,omitempty"`
	// MissingEnv son nombres, nunca valores: los valores no salen del servidor.
	MissingEnv []string       `json:"missing_env,omitempty" jsonschema:"nombres de las variables requeridas que faltan cargar en el servidor"`
	EnvUnknown bool           `json:"env_unknown,omitempty" jsonschema:"el agent no pudo mirar las variables"`
	Version    *versionOutput `json:"version,omitempty" jsonschema:"el commit que quedó corriendo según la historia de deploys"`
	LastDeploy *deployOutput  `json:"last_deploy,omitempty"`
}

type containerOutput struct {
	Name        string   `json:"name"`
	Service     string   `json:"service,omitempty"`
	Image       string   `json:"image"`
	State       string   `json:"state"`
	Health      string   `json:"health,omitempty"`
	StartedAt   string   `json:"started_at,omitempty"`
	CPUPercent  *float64 `json:"cpu_percent,omitempty"`
	MemoryBytes *int64   `json:"memory_bytes,omitempty"`
}

type serviceOutput struct {
	Unit        string   `json:"unit"`
	ActiveState string   `json:"active_state"`
	SubState    string   `json:"sub_state"`
	Result      string   `json:"result,omitempty"`
	ActiveSince string   `json:"active_since,omitempty"`
	CPUPercent  *float64 `json:"cpu_percent,omitempty"`
	MemoryBytes *int64   `json:"memory_bytes,omitempty"`
}

type siteOutput struct {
	Domain string `json:"domain"`
	Port   int    `json:"port"`
}

type versionOutput struct {
	Commit   string `json:"commit"`
	Since    string `json:"since,omitempty"`
	DeployID string `json:"deploy_id"`
}

type deployOutput struct {
	ID              string `json:"id"`
	Kind            string `json:"kind" jsonschema:"deploy, rollback o bootstrap (primer deploy)"`
	Status          string `json:"status" jsonschema:"pending, running, succeeded, failed o rolled_back (falló y volvió solo a la versión anterior)"`
	Code            string `json:"code,omitempty"`
	Message         string `json:"message,omitempty"`
	RequestedCommit string `json:"requested_commit,omitempty" jsonschema:"vacío es la punta de la rama"`
	Commit          string `json:"commit,omitempty"`
	PreviousCommit  string `json:"previous_commit,omitempty"`
	RequestedBy     string `json:"requested_by"`
	RequestedAt     string `json:"requested_at"`
	StartedAt       string `json:"started_at,omitempty"`
	FinishedAt      string `json:"finished_at,omitempty"`
	DurationSeconds *int   `json:"duration_seconds,omitempty"`
}

func (s *Server) projectStatus(ctx context.Context, call *Call, in projectStatusInput) (projectStatusOutput, error) {
	project, err := s.resolveProject(ctx, call, in.Project, tokens.CapRead)
	if err != nil {
		return projectStatusOutput{}, err
	}
	detail, err := s.backend.Project(ctx, project.ID)
	if errors.Is(err, ErrNotFound) {
		// Dejó de estar declarado entre la lista y el detalle.
		return projectStatusOutput{}, errNotFound("no hay un proyecto %q que este token alcance; list_projects da los que hay", in.Project)
	}
	if err != nil {
		return projectStatusOutput{}, err
	}
	deploys, err := s.backend.ProjectDeploys(ctx, detail.ServerID, detail.Name, versionSearch)
	if err != nil {
		return projectStatusOutput{}, err
	}

	out := projectStatusOutput{
		Project:       projectFrom(detail.ProjectInfo),
		Workdir:       detail.Workdir,
		ComposeFile:   detail.ComposeFile,
		ComposeName:   detail.ComposeProject,
		Containers:    []containerOutput{},
		Services:      []serviceOutput{},
		MissingUnits:  detail.MissingUnits,
		RuntimeSource: detail.SourceStatus,
		MissingEnv:    detail.EnvMissing,
		EnvUnknown:    detail.EnvUnknown,
		Version:       runningVersion(deploys),
	}
	if out.MissingUnits == nil {
		out.MissingUnits = []string{}
	}
	if out.RuntimeSource == "" {
		out.RuntimeSource = "unknown"
	}
	if detail.SiteDomain != "" {
		out.Site = &siteOutput{Domain: detail.SiteDomain, Port: detail.SitePort}
	}
	for _, c := range detail.Containers {
		out.Containers = append(out.Containers, containerOutput{
			Name: c.Name, Service: c.Service, Image: c.Image, State: c.State, Health: c.Health,
			StartedAt: timestamp(c.StartedAt), CPUPercent: roundPtr(c.CPUPercent), MemoryBytes: c.MemoryBytes,
		})
	}
	for _, svc := range detail.Services {
		out.Services = append(out.Services, serviceOutput{
			Unit: svc.Unit, ActiveState: svc.ActiveState, SubState: svc.SubState, Result: svc.Result,
			ActiveSince: timestamp(svc.ActiveSince), CPUPercent: roundPtr(svc.CPUPercent), MemoryBytes: svc.MemoryBytes,
		})
	}
	if len(deploys) > 0 {
		last := deployFrom(deploys[0])
		out.LastDeploy = &last
	}
	return out, nil
}

// resolveProject busca un proyecto entre los que el token alcanza con la
// capacidad. Uno que no existe y uno que no alcanza dan el mismo error.
func (s *Server) resolveProject(ctx context.Context, call *Call, ref string, capability tokens.Capability) (ProjectInfo, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ProjectInfo{}, errInvalid("falta el proyecto: servidor/nombre, su nombre o su ID, como los da list_projects")
	}
	all, err := s.backend.Projects(ctx)
	if err != nil {
		return ProjectInfo{}, err
	}
	visible := visibleProjects(call.Token, capability, all)

	var matches []ProjectInfo
	id, idErr := uuid.Parse(ref)
	serverName, name, qualified := strings.Cut(ref, "/")
	for _, p := range visible {
		switch {
		case idErr == nil && p.ID == id,
			qualified && p.ServerName == serverName && p.Name == name,
			!qualified && idErr != nil && p.Name == ref:
			matches = append(matches, p)
		}
	}
	switch len(matches) {
	case 1:
		p := matches[0]
		call.Target("project", p.ID.String())
		call.Note("proyecto", projectRef(p))
		return p, nil
	case 0:
		return ProjectInfo{}, errNotFound("no hay un proyecto %q que este token alcance; list_projects da los que hay", ref)
	default:
		refs := make([]string, len(matches))
		for i, p := range matches {
			refs[i] = projectRef(p)
		}
		slices.Sort(refs)
		return ProjectInfo{}, errInvalid("hay más de un proyecto %q: nombralo como %s", ref, strings.Join(refs, " o "))
	}
}

// runningVersion es el commit que quedó corriendo según la historia: el del
// último deploy que terminó bien, o el anterior de uno que volvió atrás. Un
// deploy que falló sin tocar nada no cambia la versión.
func runningVersion(deploys []DeployInfo) *versionOutput {
	for _, d := range deploys {
		var commit string
		switch d.Status {
		case "succeeded":
			commit = d.DeployedCommit
		case "rolled_back":
			commit = d.PreviousCommit
		default:
			continue
		}
		if commit == "" {
			continue
		}
		return &versionOutput{Commit: commit, Since: timestamp(d.FinishedAt), DeployID: d.ID.String()}
	}
	return nil
}

func projectFrom(p ProjectInfo) projectOutput {
	out := projectOutput{
		ID:          p.ID.String(),
		Ref:         projectRef(p),
		Server:      p.ServerName,
		Name:        p.Name,
		Environment: p.Environment,
		Runtime:     p.Runtime,
		Repo:        p.Repo,
		Branch:      p.Branch,
		Status:      p.Status,
		Units:       unitsOutput{Total: p.UnitsTotal, Healthy: p.UnitsHealthy},
		SeenAt:      p.SeenAt.UTC().Format(time.RFC3339),
	}
	if h := p.Health; h != nil {
		out.Healthcheck = &healthOutput{
			URL: h.URL, State: h.State, StatusCode: h.StatusCode, LatencyMS: h.LatencyMS,
			CheckedAt: timestamp(h.CheckedAt), Detail: h.Detail,
		}
	}
	return out
}

func deployFrom(d DeployInfo) deployOutput {
	out := deployOutput{
		ID:              d.ID.String(),
		Kind:            d.Kind,
		Status:          d.Status,
		Code:            d.Code,
		Message:         d.Message,
		RequestedCommit: d.RequestedCommit,
		Commit:          d.DeployedCommit,
		PreviousCommit:  d.PreviousCommit,
		RequestedBy:     d.RequestedBy,
		RequestedAt:     d.RequestedAt.UTC().Format(time.RFC3339),
		StartedAt:       timestamp(d.StartedAt),
		FinishedAt:      timestamp(d.FinishedAt),
	}
	if d.StartedAt != nil && d.FinishedAt != nil {
		seconds := int(d.FinishedAt.Sub(*d.StartedAt).Round(time.Second) / time.Second)
		out.DurationSeconds = &seconds
	}
	return out
}

func roundPtr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	r := round1(*v)
	return &r
}

// projectRef es cómo se nombra un proyecto en los mensajes.
func projectRef(p ProjectInfo) string {
	return p.ServerName + "/" + p.Name
}
