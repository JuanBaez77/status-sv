package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JuanBaez77/status-sv/internal/buildinfo"
	"github.com/JuanBaez77/status-sv/server/internal/audit"
	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/mcp"
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

func (b mcpBackend) Projects(ctx context.Context) ([]mcp.ProjectInfo, error) {
	rows, err := b.s.queries.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("listar los proyectos: %w", err)
	}
	out := make([]mcp.ProjectInfo, len(rows))
	for i, row := range rows {
		out[i] = mcp.ProjectInfo{
			ID:          row.ID,
			ServerID:    row.ServerID,
			ServerName:  row.ServerName,
			Name:        row.Name,
			Environment: row.Environment,
		}
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
