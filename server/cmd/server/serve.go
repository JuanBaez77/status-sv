package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/JuanBaez77/status-sv/internal/buildinfo"
	"github.com/JuanBaez77/status-sv/server/internal/api"
	"github.com/JuanBaez77/status-sv/server/internal/config"
	"github.com/JuanBaez77/status-sv/server/internal/metrics"
	"github.com/JuanBaez77/status-sv/server/internal/store"
	"github.com/JuanBaez77/status-sv/server/internal/users"
)

// readHeaderTimeout corta a quien abre una conexión y no manda los encabezados.
const readHeaderTimeout = 10 * time.Second

func runServe(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg)
	log.InfoContext(ctx, "arrancando el control plane",
		"version", buildinfo.Version,
		"commit", buildinfo.ShortCommit(),
		"escucha", cfg.ListenAddr,
		"base", cfg.RedactedDatabaseURL(),
	)

	pool, err := store.OpenPool(ctx, store.Config{DSN: cfg.DatabaseURL})
	if err != nil {
		return err
	}
	defer pool.Close()

	// Las particiones de métricas se crean por adelantado y se borran al
	// vencer. Se espera a que el mantenimiento termine antes de cerrar el pool
	// (los defer corren en orden inverso).
	var background sync.WaitGroup
	defer background.Wait()
	partitions := metrics.NewPartitions(store.New(pool), log)
	background.Go(func() { partitions.Run(ctx) })
	// Las sesiones vencidas del panel ya no sirven; el barrido es para que la
	// tabla no crezca.
	sessions := users.NewService(pool, cfg.TOTPKey, log)
	background.Go(func() { sessions.RunCleanup(ctx) })

	apiServer := api.New(cfg, pool, log)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		// Sin ReadTimeout ni WriteTimeout globales a propósito: el WebSocket de
		// los agents queda abierto indefinidamente. Los límites van por pedido
		// (MaxBytesHandler) y, para el WebSocket, por mensaje.
	}

	errs := make(chan error, 1)
	go func() {
		log.InfoContext(ctx, "escuchando", "direccion", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("escuchar en %s: %w", cfg.ListenAddr, err)
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.InfoContext(ctx, "apagando", "espera", cfg.ShutdownGrace, "agents_conectados", apiServer.ConnectedAgents())
		// Se les avisa a los agents antes de cortar, así reconectan con
		// backoff en vez de esperar a que se les caiga el canal.
		apiServer.CloseAgents("el control plane se está apagando")
	}

	// El contexto de apagado no puede ser el que acaba de cancelarse: hay que
	// darles tiempo a los pedidos en curso.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownGrace)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("apagar el servidor: %w", err)
	}

	log.InfoContext(shutdownCtx, "apagado limpio")
	return nil
}

// newLogger arma el registro según la configuración. En el VPS conviene JSON,
// que es lo que journald y las herramientas saben leer; en desarrollo, texto.
func newLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}

	var handler slog.Handler
	if cfg.LogJSON {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	return slog.New(handler).With("componente", "server")
}
