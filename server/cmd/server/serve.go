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
	// vencer. Las sesiones vencidas del panel ya no sirven; el barrido es para
	// que la tabla no crezca.
	partitions := metrics.NewPartitions(store.New(pool), log)
	sessions := users.NewService(pool, cfg.TOTPKey, log)
	apiServer := api.New(cfg, pool, log)
	background := []func(context.Context){
		partitions.Run,
		sessions.RunCleanup,
		// Los deploys que el agent nunca confirmó se cierran solos.
		apiServer.RunDeployExpiry,
		apiServer.RunProposalExpiry,
		apiServer.RunApprovalSweep,
	}

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		// Sin ReadTimeout ni WriteTimeout globales a propósito: el WebSocket de
		// los agents queda abierto indefinidamente. Los límites van por pedido
		// (MaxBytesHandler) y, para el WebSocket, por mensaje.
	}

	// serve espera a las tareas de fondo antes de volver, así que el pool se
	// cierra (por el defer de arriba) cuando ya nadie lo usa.
	return serve(ctx, server, log, cfg.ShutdownGrace, background, func() {
		log.InfoContext(ctx, "apagando", "espera", cfg.ShutdownGrace, "agents_conectados", apiServer.ConnectedAgents())
		// Se les avisa a los agents antes de cortar, así reconectan con
		// backoff en vez de esperar a que se les caiga el canal.
		apiServer.CloseAgents("el control plane se está apagando")
	})
}

// serve atiende con server hasta que ctx se cancela o hasta que el servidor no
// puede escuchar, y mientras tanto corre las tareas de fondo.
//
// Las tareas reciben un contexto propio, que se cancela al salir por cualquiera
// de los dos caminos. Si recibieran ctx, un error al escuchar (el puerto
// ocupado) dejaría a serve esperando para siempre a tareas que solo terminan
// con la señal de apagado. beforeShutdown corre solo en el apagado ordenado,
// antes de cortar los pedidos en curso.
func serve(ctx context.Context, server *http.Server, log *slog.Logger, grace time.Duration,
	background []func(context.Context), beforeShutdown func(),
) error {
	tasksCtx, stopTasks := context.WithCancel(ctx)
	var tasks sync.WaitGroup
	// Cancelar y después esperar, en ese orden y en un solo defer: con dos defer
	// separados, el orden inverso en que corren lo da vuelta fácil.
	defer func() {
		stopTasks()
		tasks.Wait()
	}()
	for _, task := range background {
		tasks.Go(func() { task(tasksCtx) })
	}

	errs := make(chan error, 1)
	go func() {
		log.InfoContext(ctx, "escuchando", "direccion", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("escuchar en %s: %w", server.Addr, err)
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		beforeShutdown()
	}

	// El contexto de apagado no puede ser el que acaba de cancelarse: hay que
	// darles tiempo a los pedidos en curso.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
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
