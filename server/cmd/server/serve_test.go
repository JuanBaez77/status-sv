package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// plazo es lo máximo que se espera a que serve vuelva. Si no vuelve, se colgó.
const plazo = 5 * time.Second

// blockingTask es una tarea de fondo como las de verdad: termina solo cuando
// le cancelan el contexto, y avisa en done cuando terminó.
func blockingTask(done chan<- struct{}) func(context.Context) {
	return func(ctx context.Context) {
		<-ctx.Done()
		close(done)
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Con el puerto ocupado, serve tiene que devolver el error y no quedarse
// esperando a las tareas de fondo, que solo terminan si alguien las cancela.
func TestServeReturnsWhenListenFails(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ocupar un puerto: %v", err)
	}
	defer func() { _ = busy.Close() }()

	taskDone := make(chan struct{})
	shutdownCalled := false
	server := &http.Server{Addr: busy.Addr().String(), ReadHeaderTimeout: time.Second}

	result := make(chan error, 1)
	go func() {
		result <- serve(t.Context(), server, quietLogger(), time.Second,
			[]func(context.Context){blockingTask(taskDone)},
			func() { shutdownCalled = true })
	}()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "escuchar en") {
			t.Errorf("serve() = %v, se esperaba el error al escuchar", err)
		}
	case <-time.After(plazo):
		t.Fatal("serve no volvió con el puerto ocupado: se quedó esperando a las tareas de fondo")
	}

	select {
	case <-taskDone:
	default:
		t.Error("serve volvió sin que la tarea de fondo terminara")
	}
	if shutdownCalled {
		t.Error("beforeShutdown corrió sin un apagado ordenado")
	}
}

// En el apagado ordenado, serve avisa con beforeShutdown, apaga el servidor y
// espera a las tareas de fondo antes de volver.
func TestServeShutsDownOnCancel(t *testing.T) {
	// Un puerto libre: se pide uno al sistema y se suelta enseguida.
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("buscar un puerto libre: %v", err)
	}
	addr := free.Addr().String()
	if err := free.Close(); err != nil {
		t.Fatalf("soltar el puerto: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	taskDone := make(chan struct{})
	shutdownCalled := make(chan struct{})
	server := &http.Server{
		Addr:              addr,
		Handler:           http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		ReadHeaderTimeout: time.Second,
	}

	result := make(chan error, 1)
	go func() {
		result <- serve(ctx, server, quietLogger(), time.Second,
			[]func(context.Context){blockingTask(taskDone)},
			func() { close(shutdownCalled) })
	}()

	waitListening(t, addr)
	cancel()

	select {
	case err := <-result:
		if err != nil {
			t.Errorf("serve() = %v, se esperaba un apagado limpio", err)
		}
	case <-time.After(plazo):
		t.Fatal("serve no volvió después de cancelar el contexto")
	}

	select {
	case <-taskDone:
	default:
		t.Error("serve volvió sin que la tarea de fondo terminara")
	}
	select {
	case <-shutdownCalled:
	default:
		t.Error("beforeShutdown no corrió en el apagado ordenado")
	}
}

// waitListening espera a que addr acepte conexiones, para no cancelar antes de
// que el servidor llegue a escuchar.
func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(plazo)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("el servidor no empezó a escuchar en %s", addr)
}
