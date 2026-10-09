package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JuanBaez77/status-sv/internal/buildinfo"
	"github.com/JuanBaez77/status-sv/server/internal/audit"
	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/deploys"
	"github.com/JuanBaez77/status-sv/server/internal/mcp"
	"github.com/JuanBaez77/status-sv/server/internal/store"
	"github.com/JuanBaez77/status-sv/server/internal/tokens"
)

// El MCP remoto (ADR-024), en /mcp del mismo binario.
//
// Lo que hace este archivo es la parte HTTP: rechazar los pedidos de un
// navegador, el límite por IP, el token en Authorization y el handler sin
// estado del SDK. Las herramientas están en el paquete mcp.

// Límites del MCP.
const (
	// maxMCPBody acota un pedido: una llamada a una herramienta es chica.
	maxMCPBody = 64 << 10
	// mcpRequestsPerMinute es el límite por IP, antes de mirar el token. Es
	// holgado: el que importa es el de cada token, en el paquete mcp.
	mcpRequestsPerMinute = 300
	mcpRequestsBurst     = 100
)

// mcpHandler arma el handler de /mcp.
func (s *Server) mcpHandler() http.Handler {
	server := mcp.New(mcp.Options{
		Backend:  mcpBackend{s: s},
		Recorder: s.audit,
		Log:      s.log,
		Version:  buildinfo.Version,
	})
	streamable := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server.SDK() }, &sdk.StreamableHTTPOptions{
		// Sin estado y con respuestas JSON: cada pedido se atiende solo, sin
		// sesiones que perder en un reinicio ni un SSE que el proxy retenga.
		Stateless:    true,
		JSONResponse: true,
		Logger:       s.log,
		// La protección del SDK contra DNS rebinding rechaza un Host que no
		// sea localhost cuando el control plane escucha en loopback, que es
		// justo el caso detrás de nginx. Acá la cubren el rechazo de Origin
		// (un navegador siempre lo manda en un POST) y el token obligatorio.
		DisableLocalhostProtection: true,
		MaxRequestBodyBytes:        maxMCPBody,
	})
	authed := sdkauth.RequireBearerToken(s.verifyMCPToken, nil)(streamable)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		noStore(w)
		// Ningún cliente legítimo del MCP es un navegador hasta OAuth (Fase
		// 4): así una página web no puede usar un token guardado en el
		// navegador de nadie.
		if r.Header.Get("Origin") != "" {
			writeJSON(ctx, w, http.StatusForbidden, errorResponse{Error: "forbidden", Message: "el MCP no acepta pedidos de un navegador"})
			return
		}
		if !s.mcpLimiter.allow(s.clientIP(r)) {
			writeJSON(ctx, w, http.StatusTooManyRequests, errorResponse{Error: "rate_limited", Message: "demasiados pedidos seguidos"})
			return
		}
		authed.ServeHTTP(w, r)
	})
}

// verifyMCPToken verifica el token de un pedido y lo deja en el contexto para
// las herramientas. Un token que no sirve contesta 401 y queda en la
// auditoría, como mucho una vez cada 15 minutos por IP. Un pedido sin
// Authorization lo rechaza el SDK sin llegar acá: no hay nada que atribuir.
func (s *Server) verifyMCPToken(ctx context.Context, value string, r *http.Request) (*sdkauth.TokenInfo, error) {
	ip := s.clientIP(r)
	tok, err := s.tokens.Authenticate(ctx, auth.ParseSecret(value), ip)
	if errors.Is(err, tokens.ErrUnauthenticated) {
		if s.blockedNotes.first(audit.ActionMCPAuth + "|" + ip) {
			s.record(ctx, audit.Entry{
				Actor: audit.Unknown, ActorType: audit.ActorLLM, Action: audit.ActionMCPAuth,
				TargetType: "mcp", TargetID: "-", Result: audit.ResultRejected, IP: ip,
			})
		}
		return nil, fmt.Errorf("%w: token desconocido, vencido o revocado", sdkauth.ErrInvalidToken)
	}
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo verificar un token del MCP", "error", err)
		return nil, errors.New("error interno")
	}
	return mcp.TokenInfo(tok, ip), nil
}

// mcpBackend le da al MCP los datos del control plane, con las mismas
// consultas que usa el panel.
type mcpBackend struct {
	s *Server
}

func (b mcpBackend) Servers(ctx context.Context) ([]mcp.ServerInfo, error) {
	list, err := b.s.readServers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.ServerInfo, len(list))
	for i, server := range list {
		info := mcp.ServerInfo{
			ID:           server.ID,
			Name:         server.Name,
			Hostname:     server.Hostname,
			AgentVersion: server.AgentVersion,
			Status:       server.Status,
			LastSeenAt:   server.LastSeenAt,
		}
		if sample := server.Sample; sample != nil {
			info.Sample = &mcp.SampleInfo{
				At:            sample.TS,
				CPUPercent:    float64(sample.CPUUsedPercent),
				MemUsedBytes:  sample.MemUsedBytes,
				MemTotalBytes: sample.MemTotalBytes,
				Load1:         float64(sample.Load1),
			}
			if disk := sample.WorstDisk; disk != nil {
				info.Sample.FullestDisk = &mcp.DiskInfo{Mountpoint: disk.Mountpoint, UsedPercent: disk.UsedPercent}
			}
		}
		out[i] = info
	}
	return out, nil
}

func (b mcpBackend) ServerMetrics(ctx context.Context, serverID uuid.UUID, span time.Duration) (mcp.MetricsInfo, error) {
	now := time.Now().UTC()
	q := rangeQuery{from: now.Add(-span), to: now, resolution: autoResolution(span)}
	series, err := b.s.readSeries(ctx, serverID, q)
	if err != nil {
		return mcp.MetricsInfo{}, err
	}
	out := mcp.MetricsInfo{
		Resolution: series.Resolution,
		From:       series.From,
		To:         series.To,
		Truncated:  series.Truncated,
		Points:     make([]mcp.MetricPoint, len(series.Samples)),
		Disks:      make([]mcp.DiskSeries, len(series.Disks)),
		Networks:   make([]mcp.NetworkSeries, len(series.Networks)),
	}
	for i, p := range series.Samples {
		out.Points[i] = mcp.MetricPoint{
			At:          p.TS,
			CPU:         float64(p.CPUUsedPercent),
			CPUMax:      float64(p.CPUUsedMaxPercent),
			IOWait:      float64(p.CPUIOWaitPercent),
			IOWaitMax:   float64(p.CPUIOWaitMaxPercent),
			MemUsed:     p.MemUsedBytes,
			MemUsedMax:  p.MemUsedMaxBytes,
			MemTotal:    p.MemTotalBytes,
			SwapUsed:    p.SwapUsedBytes,
			SwapUsedMax: p.SwapUsedMaxBytes,
			SwapTotal:   p.SwapTotalBytes,
			Load1:       float64(p.Load1),
		}
	}
	for i, disk := range series.Disks {
		points := make([]mcp.DiskPoint, len(disk.Points))
		for j, p := range disk.Points {
			points[j] = mcp.DiskPoint{At: p.TS, UsedBytes: p.UsedBytes, TotalBytes: p.TotalBytes}
		}
		out.Disks[i] = mcp.DiskSeries{Mountpoint: disk.Mountpoint, Points: points}
	}
	for i, network := range series.Networks {
		points := make([]mcp.NetworkPoint, len(network.Points))
		for j, p := range network.Points {
			points[j] = mcp.NetworkPoint{At: p.TS, Rx: p.RxPerSec, RxMax: p.RxMaxPerSec, Tx: p.TxPerSec, TxMax: p.TxMaxPerSec}
		}
		out.Networks[i] = mcp.NetworkSeries{Interface: network.Interface, Points: points}
	}
	return out, nil
}

func (b mcpBackend) Projects(ctx context.Context) ([]mcp.ProjectInfo, error) {
	list, err := b.s.readProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.ProjectInfo, len(list.Projects))
	for i, p := range list.Projects {
		out[i] = projectInfo(p)
	}
	return out, nil
}

func (b mcpBackend) Project(ctx context.Context, id uuid.UUID) (mcp.ProjectDetail, error) {
	row, err := b.s.queries.GetProject(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return mcp.ProjectDetail{}, mcp.ErrNotFound
	}
	if err != nil {
		return mcp.ProjectDetail{}, fmt.Errorf("leer el proyecto: %w", err)
	}
	runtime, err := b.s.readRuntime(ctx, row.ServerID)
	if err != nil {
		return mcp.ProjectDetail{}, fmt.Errorf("leer el estado de runtime: %w", err)
	}
	d := buildDetail(store.ListProjectsRow(row), runtime)
	out := mcp.ProjectDetail{
		ProjectInfo:    projectInfo(d.projectSummary),
		Workdir:        d.Workdir,
		ComposeFile:    d.ComposeFile,
		ComposeProject: d.ComposeProject,
		Containers:     make([]mcp.ContainerInfo, len(d.Containers)),
		Services:       make([]mcp.ServiceInfo, len(d.Services)),
		MissingUnits:   d.MissingUnits,
	}
	if d.Source != nil {
		out.SourceStatus = d.Source.Status
	}
	if d.Site != nil {
		out.SiteDomain, out.SitePort = d.Site.Domain, int(d.Site.Port)
	}
	if d.Env != nil {
		out.EnvRequired, out.EnvMissing, out.EnvUnknown = int(d.Env.Required), d.Env.Missing, d.Env.Unknown
	}
	for i, c := range d.Containers {
		cpu, memory := mcpUsage(c.Usage)
		out.Containers[i] = mcp.ContainerInfo{
			Name: c.Name, Service: c.ComposeService, Image: c.Image, State: c.State, Health: c.Health,
			StartedAt: c.StartedAt, CPUPercent: cpu, MemoryBytes: memory,
		}
	}
	for i, svc := range d.Services {
		cpu, memory := mcpUsage(svc.Usage)
		out.Services[i] = mcp.ServiceInfo{
			Unit: svc.Unit, ActiveState: svc.ActiveState, SubState: svc.SubState, Result: svc.Result,
			ActiveSince: svc.ActiveSince, CPUPercent: cpu, MemoryBytes: memory,
		}
	}
	return out, nil
}

func (b mcpBackend) ProjectDeploys(ctx context.Context, serverID uuid.UUID, project string, limit int) ([]mcp.DeployInfo, error) {
	rows, err := b.s.deploys.History(ctx, serverID, project, int32(min(limit, historyLimit))) //nolint:gosec // acotado por historyLimit
	if err != nil {
		return nil, fmt.Errorf("leer la historia de deploys: %w", err)
	}
	out := make([]mcp.DeployInfo, len(rows))
	for i, d := range rows {
		out[i] = mcp.DeployInfo{
			ID: d.ID, Kind: d.Kind, Status: d.Status, Code: d.Code, Message: d.Message,
			RequestedCommit: d.RequestedCommit, DeployedCommit: d.DeployedCommit, PreviousCommit: d.PreviousCommit,
			RequestedBy: d.RequestedByName, RequestedAt: d.RequestedAt, StartedAt: d.StartedAt, FinishedAt: d.FinishedAt,
		}
	}
	return out, nil
}

func projectInfo(p projectSummary) mcp.ProjectInfo {
	out := mcp.ProjectInfo{
		ID: p.ID, ServerID: p.ServerID, ServerName: p.ServerName, Name: p.Name,
		Environment: p.Environment, Runtime: p.Runtime, Repo: p.Repo, Branch: p.Branch,
		Status: string(p.Status), UnitsTotal: p.Units.Total, UnitsHealthy: p.Units.Healthy, SeenAt: p.SeenAt,
	}
	if h := p.Health; h != nil {
		out.Health = &mcp.HealthInfo{
			URL: h.URL, State: h.State, StatusCode: int(h.StatusCode), LatencyMS: int(h.LatencyMS),
			CheckedAt: h.CheckedAt, Detail: h.Detail,
		}
	}
	return out
}

func mcpUsage(u *runtimeUsage) (*float64, *int64) {
	if u == nil {
		return nil, nil
	}
	cpu, memory := float64(u.CPUPercent), u.MemoryBytes
	return &cpu, &memory
}

func (b mcpBackend) Deploy(ctx context.Context, id uuid.UUID) (mcp.DeployDetail, error) {
	d, err := b.s.deploys.Get(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return mcp.DeployDetail{}, mcp.ErrNotFound
	}
	if err != nil {
		return mcp.DeployDetail{}, fmt.Errorf("leer el deploy: %w", err)
	}
	steps := deploys.Steps(d.Steps)
	out := mcp.DeployDetail{
		DeployInfo: mcp.DeployInfo{
			ID: d.ID, Kind: d.Kind, Status: d.Status, Code: d.Code, Message: d.Message,
			RequestedCommit: d.RequestedCommit, DeployedCommit: d.DeployedCommit, PreviousCommit: d.PreviousCommit,
			RequestedBy: d.RequestedByName, RequestedAt: d.RequestedAt, StartedAt: d.StartedAt, FinishedAt: d.FinishedAt,
		},
		ProjectID: d.ProjectID, ProjectName: d.ProjectName, ServerID: d.ServerID, ServerName: d.ServerName,
		Environment: d.Environment,
		Steps:       make([]mcp.StepInfo, len(steps)),
	}
	for i, step := range steps {
		out.Steps[i] = mcp.StepInfo{
			Name: string(step.Name), Service: step.Service, Status: string(step.Status),
			StartedAt: nonZero(step.StartedAt), FinishedAt: nonZero(step.FinishedAt),
			Detail: step.Detail, Output: step.Output,
		}
	}
	return out, nil
}

func nonZero(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
