package mcp

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Backend es lo que el MCP lee del control plane. Lo implementa la API, con
// las mismas consultas que usa el panel.
type Backend interface {
	// Servers devuelve todos los servidores con su estado y su última muestra.
	Servers(ctx context.Context) ([]ServerInfo, error)
	// Projects devuelve todos los proyectos declarados.
	Projects(ctx context.Context) ([]ProjectInfo, error)
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

// ProjectInfo es un proyecto declarado.
type ProjectInfo struct {
	ID          uuid.UUID
	ServerID    uuid.UUID
	ServerName  string
	Name        string
	Environment string
}
