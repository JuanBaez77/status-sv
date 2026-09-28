package runtimestate

import (
	"context"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/proto"
)

func TestSignatureIgnoresNoise(t *testing.T) {
	base := snap(t0)
	base.Containers = []proto.Container{{
		ID: containerID(1), Name: "web", Image: "nginx", State: proto.ContainerRunning,
		Usage: &proto.Usage{CPUPercent: 1.2, MemoryBytes: 50<<20 + 1000},
	}}

	// Otra marca y un consumo que baila unas décimas: la misma foto.
	noise := snap(t0.Add(time.Minute))
	noise.Containers = []proto.Container{base.Containers[0]}
	noise.Containers[0].Usage = &proto.Usage{CPUPercent: 1.4, MemoryBytes: 50<<20 + 900_000}
	if signature(base) != signature(noise) {
		t.Error("la marca y el ruido del consumo no son un cambio")
	}

	// Un cambio de estado sí.
	stopped := snap(t0)
	stopped.Containers = []proto.Container{base.Containers[0]}
	stopped.Containers[0].State = proto.ContainerExited
	if signature(base) == signature(stopped) {
		t.Error("un contenedor detenido es un cambio")
	}

	// Y un salto de consumo también.
	busy := snap(t0)
	busy.Containers = []proto.Container{base.Containers[0]}
	busy.Containers[0].Usage = &proto.Usage{CPUPercent: 40, MemoryBytes: 50 << 20}
	if signature(base) == signature(busy) {
		t.Error("pasar de 1% a 40% de CPU es un cambio")
	}

	// La firma no toca la foto original.
	if base.Containers[0].Usage.CPUPercent != 1.2 {
		t.Error("signature modificó la foto")
	}
}

// fakeWatcher avisa un cambio cuando se lo piden.
type fakeWatcher struct{ trigger chan struct{} }

func (w *fakeWatcher) Watch(ctx context.Context, changed func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.trigger:
			changed()
		}
	}
}

func TestRunnerSendsOnStartAndOnChanges(t *testing.T) {
	f := newFixture()
	outbox := NewOutbox()
	watcher := &fakeWatcher{trigger: make(chan struct{})}
	r := NewRunner(f.a, outbox, quietLog(), watcher)
	r.settle, r.refresh = 10*time.Millisecond, time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	waitPut := func(what string) proto.RuntimeState {
		t.Helper()
		select {
		case <-outbox.Wake():
		case <-time.After(5 * time.Second):
			t.Fatalf("no salió la foto: %s", what)
		}
		s, ok := outbox.Next(time.Now())
		if !ok {
			t.Fatalf("la bandeja está vacía: %s", what)
		}
		outbox.Sent([16]byte{1}, s.TakenAt, time.Now())
		outbox.Acked(s.TakenAt)
		return s
	}

	waitPut("al arrancar")

	// Un cambio de verdad sale.
	f.docker.containers[0].State = proto.ContainerExited
	watcher.trigger <- struct{}{}
	if s := waitPut("después de un cambio"); s.Containers[0].State != proto.ContainerExited {
		t.Errorf("la foto no trae el cambio: %+v", s.Containers[0])
	}

	// Un aviso sin cambio (un evento que no mueve nada) no manda otra foto.
	watcher.trigger <- struct{}{}
	select {
	case <-outbox.Wake():
		t.Fatal("una foto igual a la anterior no tiene que salir")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run no volvió al cancelar el context")
	}
}
