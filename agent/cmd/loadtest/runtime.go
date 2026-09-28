package main

import (
	"context"
	"fmt"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/runtimestate"
	"github.com/JuanBaez77/status-sv/proto"
)

// Tamaño de la foto de runtime de cada agent simulado: un VPS con varios
// proyectos de clientes.
const (
	syntheticContainers = 20
	syntheticServices   = 30
	syntheticProcesses  = 5
)

// feedRuntime deja una foto inventada en la bandeja al arrancar y en cada
// refresco, como el Runner del agent de verdad. El consumo cambia en cada
// una, así que todas salen: es el peor caso para el control plane.
func feedRuntime(ctx context.Context, number int, outbox *runtimestate.Outbox) {
	tick := time.NewTicker(runtimestate.DefaultRefresh)
	defer tick.Stop()
	for round := 0; ; round++ {
		outbox.Put(syntheticRuntime(number, round, time.Now()))
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func syntheticRuntime(number, round int, now time.Time) proto.RuntimeState {
	ok := proto.RuntimeSource{Status: proto.SourceOK}
	s := proto.RuntimeState{
		TakenAt: now.UTC().Truncate(time.Millisecond),
		Sources: proto.RuntimeSources{Docker: ok, Systemd: ok, Processes: ok},
	}
	usage := func(i int) *proto.Usage {
		return &proto.Usage{CPUPercent: float64((i+round)%10) / 2, MemoryBytes: uint64(50+i+round%7) << 20} //nolint:gosec // números chicos inventados para la carga
	}
	for i := range syntheticContainers {
		s.Containers = append(s.Containers, proto.Container{
			ID:             fmt.Sprintf("%032x%032x", number, i),
			Name:           fmt.Sprintf("cliente-%02d-web-%d", i/2, i%2),
			Image:          fmt.Sprintf("ghcr.io/cliente-%02d/web:2026.09.%02d", i/2, 1+i%28),
			State:          proto.ContainerRunning,
			Health:         proto.HealthHealthy,
			StartedAt:      now.Add(-time.Duration(i) * time.Hour).UTC().Truncate(time.Second),
			ComposeProject: fmt.Sprintf("cliente-%02d", i/2),
			ComposeService: "web",
			Usage:          usage(i),
		})
	}
	for i := range syntheticServices {
		s.Services = append(s.Services, proto.Service{
			Unit: fmt.Sprintf("servicio-%02d.service", i), LoadState: "loaded",
			ActiveState: proto.ActiveActive, SubState: "running", MainPID: 1000 + i, Usage: usage(i),
		})
	}
	for i := range syntheticProcesses {
		s.Processes = append(s.Processes, proto.Process{
			PID: 5000 + i, Name: "python3", Command: "python3 tarea.py --lote 500", User: "deploy", Usage: *usage(i),
		})
	}
	return s
}
