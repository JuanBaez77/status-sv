// Package store es el único camino del control plane hacia PostgreSQL. Las
// consultas las genera sqlc a partir de los archivos de server/internal/store/queries,
// así que no hay SQL armado por concatenación en ninguna parte del servidor.
//
// Este archivo no lo genera sqlc: agrega la apertura del pool de conexiones.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config son los parámetros del pool de conexiones.
type Config struct {
	// DSN es la cadena de conexión de PostgreSQL.
	DSN string
	// MaxConns es el máximo de conexiones abiertas. Si es cero, se usan 10.
	MaxConns int32
	// ConnectTimeout limita lo que puede tardar la primera conexión. Si es cero,
	// se usan 10 segundos.
	ConnectTimeout time.Duration
}

const (
	defaultMaxConns       = 10
	defaultConnectTimeout = 10 * time.Second
	maxConnLifetime       = time.Hour
	maxConnIdleTime       = 15 * time.Minute
)

// OpenPool abre el pool y verifica que la base responda antes de devolverlo.
// Quien lo abre es responsable de cerrarlo.
func OpenPool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("leer la cadena de conexión: %w", err)
	}

	poolCfg.MaxConns = defaultMaxConns
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	poolCfg.MaxConnLifetime = maxConnLifetime
	poolCfg.MaxConnIdleTime = maxConnIdleTime

	timeout := defaultConnectTimeout
	if cfg.ConnectTimeout > 0 {
		timeout = cfg.ConnectTimeout
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("abrir el pool de conexiones: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("conectar con PostgreSQL: %w", err)
	}

	return pool, nil
}
