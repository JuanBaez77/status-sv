package mcp

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound es lo que devuelve Backend cuando lo pedido no existe.
var ErrNotFound = errors.New("no existe")

// Backend es lo que el MCP lee del control plane. Lo implementa la API, con
// las mismas consultas que usa el panel.
type Backend interface {
	// Servers devuelve todos los servidores con su estado y su última muestra.
	Servers(ctx context.Context) ([]ServerInfo, error)
	// Projects devuelve todos los proyectos declarados, con su estado.
	Projects(ctx context.Context) ([]ProjectInfo, error)
	// Project devuelve un proyecto con lo que corre de él, o ErrNotFound.
	Project(ctx context.Context, id uuid.UUID) (ProjectDetail, error)
	// ProjectDeploys devuelve los últimos deploys de un proyecto, del más
	// nuevo al más viejo.
	ProjectDeploys(ctx context.Context, serverID uuid.UUID, project string, limit int) ([]DeployInfo, error)
	// ServerMetrics devuelve las series de un servidor desde span atrás hasta
	// ahora, con la resolución que corresponde al largo del rango.
	ServerMetrics(ctx context.Context, serverID uuid.UUID, span time.Duration) (MetricsInfo, error)
}

// ServerInfo es un servidor con su estado derivado del último contacto.
type ServerInfo struct {
	ID           uuid.UUID
	Name         string
	Hostname     string
	AgentVersion string
	// Status es online, offline o never_seen.
	Status     string
	LastSeenAt *time.Time
	// Sample es la última muestra de los últimos minutos, o nil.
	Sample *SampleInfo
}

// SampleInfo es el resumen de la última muestra de un servidor.
type SampleInfo struct {
	At            time.Time
	CPUPercent    float64
	MemUsedBytes  int64
	MemTotalBytes int64
	Load1         float64
	// FullestDisk es el disco más lleno de esa misma muestra, o nil.
	FullestDisk *DiskInfo
}

// DiskInfo es un disco y cuánto está usado.
type DiskInfo struct {
	Mountpoint  string
	UsedPercent float64
}

// ProjectInfo es un proyecto declarado, con su estado derivado como lo
// muestra la lista del panel.
type ProjectInfo struct {
	ID          uuid.UUID
	ServerID    uuid.UUID
	ServerName  string
	Name        string
	Environment string
	Runtime     string
	Repo        string
	Branch      string
	// Status es ok, degraded, down o unknown.
	Status       string
	UnitsTotal   int
	UnitsHealthy int
	// Health es el healthcheck declarado y su último resultado, o nil.
	Health *HealthInfo
	SeenAt time.Time
}

// HealthInfo es el healthcheck de un proyecto.
type HealthInfo struct {
	URL        string
	State      string
	StatusCode int
	LatencyMS  int
	CheckedAt  *time.Time
	Detail     string
}

// ProjectDetail es un proyecto con lo que corre de él en su servidor.
type ProjectDetail struct {
	ProjectInfo
	Workdir        string
	ComposeFile    string
	ComposeProject string
	Containers     []ContainerInfo
	Services       []ServiceInfo
	// MissingUnits son los servicios declarados que el agent no ve.
	MissingUnits []string
	// SourceStatus dice si el agent pudo leer Docker o systemd: ok, error o
	// vacío si nunca mandó nada.
	SourceStatus string
	SiteDomain   string
	SitePort     int
	// EnvMissing son los nombres de las variables requeridas que faltan.
	EnvRequired int
	EnvMissing  []string
	EnvUnknown  bool
}

// ContainerInfo es un contenedor de un proyecto Docker.
type ContainerInfo struct {
	Name        string
	Service     string
	Image       string
	State       string
	Health      string
	StartedAt   *time.Time
	CPUPercent  *float64
	MemoryBytes *int64
}

// ServiceInfo es una unidad de un proyecto systemd.
type ServiceInfo struct {
	Unit        string
	ActiveState string
	SubState    string
	Result      string
	ActiveSince *time.Time
	CPUPercent  *float64
	MemoryBytes *int64
}

// DeployInfo es un deploy de la historia de un proyecto, sin sus pasos.
type DeployInfo struct {
	ID              uuid.UUID
	Kind            string
	Status          string
	Code            string
	Message         string
	RequestedCommit string
	DeployedCommit  string
	PreviousCommit  string
	RequestedBy     string
	RequestedAt     time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
}
