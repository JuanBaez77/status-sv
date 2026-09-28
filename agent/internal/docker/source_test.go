package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/proto"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSnapshotTranslatesToContract(t *testing.T) {
	source := NewSource(startFake(t, newFakeDocker()), quietLog())
	containers, src := source.Snapshot(context.Background())
	if src.Status != proto.SourceOK {
		t.Fatalf("fuente: %+v", src)
	}
	if len(containers) != 2 {
		t.Fatalf("esperaba 2 contenedores, hay %d", len(containers))
	}

	web, db := containers[0], containers[1]
	want := proto.Container{
		ID:             idWeb,
		Name:           "cristales-web-1",
		Image:          "ghcr.io/extendsystem/cristales:2026.09.20",
		State:          proto.ContainerRunning,
		Health:         proto.HealthHealthy,
		StartedAt:      time.Date(2026, 9, 27, 9, 0, 0, 123456789, time.UTC),
		ComposeProject: "cristales",
		ComposeService: "web",
	}
	if fmt.Sprintf("%+v", web) != fmt.Sprintf("%+v", want) {
		t.Errorf("web:\n obtuve   %+v\n esperaba %+v", web, want)
	}

	// El alias del enlace ("/cristales-web-1/db") se saltea; el año 1 de
	// Docker es "nunca arrancó" y queda vacío.
	if db.Name != "cristales-db-1" || db.State != proto.ContainerExited || !db.StartedAt.IsZero() || db.Health != proto.HealthNone {
		t.Errorf("db mal traducido: %+v", db)
	}

	// Lo que sale del adaptador tiene que pasar la validación del contrato.
	for _, c := range containers {
		if err := c.Validate(); err != nil {
			t.Errorf("el contenedor %s no pasa el contrato: %v", c.Name, err)
		}
	}
}

func TestSnapshotCachesInspections(t *testing.T) {
	fake := newFakeDocker()
	source := NewSource(startFake(t, fake), quietLog())
	ctx := context.Background()

	source.Snapshot(ctx)
	source.Snapshot(ctx)
	if n := fake.inspections(idWeb); n != 1 {
		t.Fatalf("sin cambios, el contenedor se inspeccionó %d veces", n)
	}

	// Un cambio de estado en la lista vuelve a inspeccionar.
	fake.mu.Lock()
	fake.list[0]["State"] = "restarting"
	fake.mu.Unlock()
	source.Snapshot(ctx)
	if n := fake.inspections(idWeb); n != 2 {
		t.Fatalf("con el estado cambiado esperaba 2 inspecciones, hubo %d", n)
	}

	// Un evento del contenedor también, aunque el estado siga igual (un
	// reinicio rápido que vuelve a running entre dos fotos).
	source.invalidate(idWeb)
	source.Snapshot(ctx)
	if n := fake.inspections(idWeb); n != 3 {
		t.Fatalf("después de un evento esperaba 3 inspecciones, hubo %d", n)
	}
}

func TestSnapshotForgetsRemovedContainers(t *testing.T) {
	fake := newFakeDocker()
	source := NewSource(startFake(t, fake), quietLog())
	source.Snapshot(context.Background())

	fake.mu.Lock()
	fake.list = fake.list[:1]
	fake.mu.Unlock()
	source.Snapshot(context.Background())

	source.mu.Lock()
	defer source.mu.Unlock()
	if _, ok := source.details[idDB]; ok {
		t.Error("el caché conserva un contenedor que ya no existe")
	}
}

// Si un contenedor se borra entre la lista y la inspección, igual aparece,
// solo que sin hora de arranque.
func TestSnapshotSurvivesFailedInspection(t *testing.T) {
	fake := newFakeDocker()
	delete(fake.inspect, idWeb)
	source := NewSource(startFake(t, fake), quietLog())

	containers, src := source.Snapshot(context.Background())
	if src.Status != proto.SourceOK || len(containers) != 2 {
		t.Fatalf("fuente %+v con %d contenedores", src, len(containers))
	}
	if !containers[0].StartedAt.IsZero() {
		t.Errorf("sin inspección no hay hora de arranque: %v", containers[0].StartedAt)
	}
}

func TestUnknownStatesAreMapped(t *testing.T) {
	if got := containerState("hibernando"); got != proto.ContainerUnknown {
		t.Errorf("un estado nuevo de Docker tiene que ser unknown, obtuve %q", got)
	}
	var i Inspection
	i.State.Health = &struct {
		Status string `json:"Status"`
	}{Status: "none"}
	if got := containerHealth(i); got != proto.HealthNone {
		t.Errorf(`"none" es sin healthcheck, obtuve %q`, got)
	}
}

func TestSourceFromError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want proto.SourceStatus
	}{
		{name: "sin Docker", err: fmt.Errorf("listar: %w", ErrNoSocket), want: proto.SourceUnavailable},
		{name: "sin permiso", err: fmt.Errorf("listar: %w", ErrPermissionDenied), want: proto.SourceError},
		{name: "tiempo agotado", err: context.DeadlineExceeded, want: proto.SourceError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := SourceFromError(tt.err)
			if src.Status != tt.want {
				t.Errorf("obtuve %q, esperaba %q", src.Status, tt.want)
			}
			if err := src.Validate(); err != nil {
				t.Errorf("la fuente no pasa el contrato: %v", err)
			}
		})
	}
}

// Un error largo, con saltos de línea y acentos, se recorta sin romper el
// UTF-8 y sin caracteres de control, para que la foto no se rechace.
func TestSourceDetailFitsTheContract(t *testing.T) {
	err := errors.New(strings.Repeat("línea\n", 100))
	src := SourceFromError(err)
	if len(src.Detail) > proto.MaxSourceDetailLen {
		t.Errorf("la explicación ocupa %d bytes", len(src.Detail))
	}
	if err := src.Validate(); err != nil {
		t.Errorf("no pasa el contrato: %v", err)
	}
}

func TestWatchSignalsOnOpenAndOnEvents(t *testing.T) {
	fake := newFakeDocker()
	fake.events = []Event{{Type: "container", Action: "die", Actor: struct {
		ID string `json:"ID"`
	}{ID: idWeb}}}
	source := NewSource(startFake(t, fake), quietLog())
	source.retryFirst, source.retryCap = time.Millisecond, 5*time.Millisecond

	// Algo en el caché, para ver que el stream nuevo lo vacía.
	source.Snapshot(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	changes := make(chan struct{}, 100)
	done := make(chan struct{})
	go func() {
		source.Watch(ctx, func() { changes <- struct{}{} })
		close(done)
	}()

	// El fake corta el stream después de cada tanda, así que Watch reabre:
	// cada apertura avisa y cada evento avisa. Con tres avisos ya hubo al
	// menos una reapertura.
	for i := range 3 {
		select {
		case <-changes:
		case <-time.After(5 * time.Second):
			t.Fatalf("no llegó el aviso %d", i+1)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch no volvió al cancelar el context")
	}

	source.mu.Lock()
	defer source.mu.Unlock()
	if len(source.details) != 0 {
		t.Errorf("abrir el stream tendría que vaciar el caché; quedan %d", len(source.details))
	}
}
