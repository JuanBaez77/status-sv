package main

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// logCounts cuenta lo que salió mal durante la prueba. Un aviso es una
// reconexión o un lote rechazado; un error es una credencial que el control
// plane no aceptó. Con diez agents y un control plane sano, los dos tienen que
// quedar en cero.
type logCounts struct {
	warnings atomic.Int64
	errors   atomic.Int64
}

// countingHandler lleva la cuenta y deja pasar el registro al de abajo.
type countingHandler struct {
	inner  slog.Handler
	counts *logCounts
}

func newCountingHandler(inner slog.Handler, counts *logCounts) slog.Handler {
	return &countingHandler{inner: inner, counts: counts}
}

func (h *countingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	// El conteo no depende de lo que se imprima: un aviso cuenta aunque el nivel
	// de registro lo esconda.
	return level >= slog.LevelWarn || h.inner.Enabled(ctx, level)
}

func (h *countingHandler) Handle(ctx context.Context, record slog.Record) error {
	switch {
	case record.Level >= slog.LevelError:
		h.counts.errors.Add(1)
	case record.Level >= slog.LevelWarn:
		h.counts.warnings.Add(1)
	}
	if !h.inner.Enabled(ctx, record.Level) {
		return nil
	}
	return h.inner.Handle(ctx, record)
}

func (h *countingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &countingHandler{inner: h.inner.WithAttrs(attrs), counts: h.counts}
}

func (h *countingHandler) WithGroup(name string) slog.Handler {
	return &countingHandler{inner: h.inner.WithGroup(name), counts: h.counts}
}
