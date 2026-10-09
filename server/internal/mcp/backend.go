package mcp

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Errores de Backend.
var (
	// ErrAgentOffline es un servidor cuyo agent no está conectado.
	ErrAgentOffline = errors.New("el agent de ese servidor no está conectado")
	// ErrNoAnswer es un agent que no contestó a tiempo.
	ErrNoAnswer = errors.New("el agent no contestó a tiempo")
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
	// Deploy devuelve un deploy con sus pasos, o ErrNotFound.
	Deploy(ctx context.Context, id uuid.UUID) (DeployDetail, error)
	// Logs le pide al agent del servidor los últimos renglones de un
	// contenedor (por su ID) o de un servicio. Puede devolver ErrAgentOffline
	// o ErrNoAnswer.
	Logs(ctx context.Context, serverID uuid.UUID, kind, target string, lines int) (LogsInfo, error)
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
	// ID es el ID completo, con el que se le piden los logs al agent.
	ID          string
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

// DeployDetail es un deploy con sus pasos.
type DeployDetail struct {
	DeployInfo
	// ProjectID es nil si el proyecto dejó de estar declarado.
	ProjectID   *uuid.UUID
	ProjectName string
	ServerID    uuid.UUID
	ServerName  string
	Environment string
	Steps       []StepInfo
}

// StepInfo es un paso de un deploy. Output son los últimos renglones de su
// salida, enmascarados en el agent: texto del servidor, no confiable.
type StepInfo struct {
	Name       string
	Service    string
	Status     string
	StartedAt  *time.Time
	FinishedAt *time.Time
	Detail     string
	Output     []string
}

// LogsInfo es la respuesta del agent a un pedido de logs.
type LogsInfo struct {
	// Status es ok, rejected o failed.
	Status    string
	Code      string
	Message   string
	Lines     []LogLine
	Truncated bool
}

// LogLine es un renglón de log, ya enmascarado en el agent.
type LogLine struct {
	At     *time.Time
	Stream string
	Text   string
}
