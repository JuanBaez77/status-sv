package mcp

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// limitClass separa las herramientas con un límite propio además del general.
type limitClass int

const (
	// classRead son las lecturas comunes: solo el límite general.
	classRead limitClass = iota
	// classLogs es get_logs, que además le pide algo al agent del servidor.
	classLogs
	// classWrite son las escrituras: cada una le pide a una persona que
	// apruebe algo, así que van con un cupo chico (ADR-026).
	classWrite
)

// Límites por token (ADR-024).
const (
	callsPerMinute = 60
	callsBurst     = 20
	logsPerMinute  = 10
	logsBurst      = 5
	// writesPerHour son los pedidos de escritura por hora de un token.
	writesPerHour = 6
	writesBurst   = 6
	// idleTTL es cuánto se recuerda un token sin llamadas.
	idleTTL = 15 * time.Minute
	// blockedNoteEvery es cada cuánto se anota en audit_log que un token está
	// frenado por el límite: un LLM en un bucle no tiene que llenar la tabla.
	blockedNoteEvery = time.Minute
)

type tokenLimits struct {
	calls       *rate.Limiter
	logs        *rate.Limiter
	writes      *rate.Limiter
	lastSeen    time.Time
	lastBlocked time.Time
}

// limiter lleva el cupo de cada token, en memoria. Se olvida de los tokens que
// dejan de llamar.
type limiter struct {
	mu     sync.Mutex
	tokens map[uuid.UUID]*tokenLimits
	lastGC time.Time
}

func newLimiter() *limiter {
	return &limiter{tokens: make(map[uuid.UUID]*tokenLimits)}
}

// allow dice si el token puede hacer una llamada más de esa clase, y la cuenta.
func (l *limiter) allow(id uuid.UUID, class limitClass, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.collect(now)

	t, ok := l.tokens[id]
	if !ok {
		t = &tokenLimits{
			calls:  rate.NewLimiter(rate.Limit(callsPerMinute/60.0), callsBurst),
			logs:   rate.NewLimiter(rate.Limit(logsPerMinute/60.0), logsBurst),
			writes: rate.NewLimiter(rate.Limit(writesPerHour/3600.0), writesBurst),
		}
		l.tokens[id] = t
	}
	t.lastSeen = now

	// Primero el de la clase: si no hay cupo ahí, la llamada no gasta del
	// general.
	switch class {
	case classLogs:
		if !t.logs.AllowN(now, 1) {
			return false
		}
	case classWrite:
		if !t.writes.AllowN(now, 1) {
			return false
		}
	}
	return t.calls.AllowN(now, 1)
}

// noteBlocked dice si hay que anotar en audit_log que el token quedó frenado:
// como mucho una vez por minuto.
func (l *limiter) noteBlocked(id uuid.UUID, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.tokens[id]
	if !ok || now.Sub(t.lastBlocked) < blockedNoteEvery {
		return false
	}
	t.lastBlocked = now
	return true
}

func (l *limiter) collect(now time.Time) {
	if now.Sub(l.lastGC) < idleTTL {
		return
	}
	l.lastGC = now
	for id, t := range l.tokens {
		if now.Sub(t.lastSeen) > idleTTL {
			delete(l.tokens, id)
		}
	}
}
