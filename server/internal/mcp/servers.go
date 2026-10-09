package mcp

import (
	"context"
	"math"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JuanBaez77/status-sv/server/internal/tokens"
)

// readOnly son las anotaciones de toda herramienta del Sprint 7: no cambian
// nada y solo miran el control plane.
func readOnly(title string) *sdk.ToolAnnotations {
	closed := false
	return &sdk.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &closed}
}

func (s *Server) addServerTools() {
	addTool(s, &sdk.Tool{
		Name: "list_servers",
		Description: "Lista los servidores que ve este token: estado (online, offline o never_seen), " +
			"último contacto, versión del agent, el uso de CPU, memoria y disco de la última muestra, " +
			"y los proyectos de cada uno que el token alcanza.",
		Annotations: readOnly("Servidores"),
	}, classRead, s.listServers)
}

type listServersInput struct{}

type listServersOutput struct {
	Servers []serverOutput `json:"servers"`
}

type serverOutput struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Hostname     string `json:"hostname"`
	Status       string `json:"status" jsonschema:"online: latió hace poco; offline: sin latido hace más de 45 s; never_seen: nunca se conectó"`
	LastSeenAt   string `json:"last_seen_at,omitempty" jsonschema:"último contacto, en RFC 3339 y UTC"`
	AgentVersion string `json:"agent_version"`
	// Projects son los proyectos del servidor que el token alcanza.
	Projects []string     `json:"projects" jsonschema:"los proyectos de este servidor que el token alcanza"`
	Usage    *usageOutput `json:"usage,omitempty" jsonschema:"la última muestra de los últimos 15 minutos; falta si el servidor no mandó ninguna"`
}

type usageOutput struct {
	At               string      `json:"at"`
	CPUPercent       float64     `json:"cpu_percent"`
	MemoryPercent    float64     `json:"memory_percent"`
	MemoryUsedBytes  int64       `json:"memory_used_bytes"`
	MemoryTotalBytes int64       `json:"memory_total_bytes"`
	Load1            float64     `json:"load1"`
	FullestDisk      *diskOutput `json:"fullest_disk,omitempty"`
}

type diskOutput struct {
	Mountpoint  string  `json:"mountpoint"`
	UsedPercent float64 `json:"used_percent"`
}

func (s *Server) listServers(ctx context.Context, call *Call, _ listServersInput) (listServersOutput, error) {
	if !call.Token.CanRead() {
		return listServersOutput{}, errDenied("este token no tiene permiso de lectura: no ve servidores")
	}
	servers, err := s.backend.Servers(ctx)
	if err != nil {
		return listServersOutput{}, err
	}
	projects, err := s.backend.Projects(ctx)
	if err != nil {
		return listServersOutput{}, err
	}

	visible := visibleProjects(call.Token, tokens.CapRead, projects)
	byServer := make(map[string][]string)
	for _, p := range visible {
		key := p.ServerID.String()
		byServer[key] = append(byServer[key], p.Name)
	}

	out := listServersOutput{Servers: []serverOutput{}}
	for _, server := range servers {
		names := byServer[server.ID.String()]
		// Un token con lista de proyectos ve solo los servidores donde están.
		if !call.Token.AllProjects() && len(names) == 0 {
			continue
		}
		if names == nil {
			names = []string{}
		}
		item := serverOutput{
			ID:           server.ID.String(),
			Name:         server.Name,
			Hostname:     server.Hostname,
			Status:       server.Status,
			LastSeenAt:   timestamp(server.LastSeenAt),
			AgentVersion: server.AgentVersion,
			Projects:     names,
		}
		if sample := server.Sample; sample != nil {
			item.Usage = &usageOutput{
				At:               sample.At.UTC().Format(time.RFC3339),
				CPUPercent:       round1(sample.CPUPercent),
				MemoryPercent:    round1(ratio(sample.MemUsedBytes, sample.MemTotalBytes)),
				MemoryUsedBytes:  sample.MemUsedBytes,
				MemoryTotalBytes: sample.MemTotalBytes,
				Load1:            round2(sample.Load1),
			}
			if disk := sample.FullestDisk; disk != nil {
				item.Usage.FullestDisk = &diskOutput{Mountpoint: disk.Mountpoint, UsedPercent: round1(disk.UsedPercent)}
			}
		}
		out.Servers = append(out.Servers, item)
	}
	call.Note("servidores", len(out.Servers))
	return out, nil
}

// visibleProjects devuelve los proyectos que el token alcanza con la capacidad.
func visibleProjects(tok tokens.Token, capability tokens.Capability, all []ProjectInfo) []ProjectInfo {
	out := make([]ProjectInfo, 0, len(all))
	for _, p := range all {
		if tok.AllowsProject(capability, p.Environment, p.ID) {
			out = append(out, p)
		}
	}
	return out
}

func timestamp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func ratio(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func round2(v float64) float64 { return math.Round(v*100) / 100 }
