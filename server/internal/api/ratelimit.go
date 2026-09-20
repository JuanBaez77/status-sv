package api

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Límites del enrolamiento. Es el endpoint que hay que proteger: acepta un
// secreto sin autenticación previa, así que es donde alguien probaría tokens.
// Un enrolamiento real es un evento raro, de a uno por servidor.
const (
	enrollPerMinute = 5
	enrollBurst     = 3
	// limiterTTL es cuánto se recuerda una IP sin actividad antes de olvidarla.
	limiterTTL = 15 * time.Minute
)

// ipLimiter limita pedidos por IP de origen, con olvido de las IPs inactivas
// para que el mapa no crezca sin control.
type ipLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	limit    rate.Limit
	burst    int
	ttl      time.Duration
	lastGC   time.Time
	now      func() time.Time // se reemplaza en los tests
}

type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newIPLimiter(perMinute float64, burst int, ttl time.Duration) *ipLimiter {
	return &ipLimiter{
		visitors: make(map[string]*visitor),
		limit:    rate.Limit(perMinute / 60),
		burst:    burst,
		ttl:      ttl,
		now:      time.Now,
	}
}

// allow dice si la IP puede hacer un pedido más.
func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.collectGarbage(now)

	found, ok := l.visitors[ip]
	if !ok {
		found = &visitor{limiter: rate.NewLimiter(l.limit, l.burst)}
		l.visitors[ip] = found
	}
	found.lastSeen = now

	return found.limiter.Allow()
}

// collectGarbage olvida las IPs inactivas. Se llama con el candado tomado y
// barre como mucho una vez por ttl, así no cuesta nada en el caso normal.
func (l *ipLimiter) collectGarbage(now time.Time) {
	if now.Sub(l.lastGC) < l.ttl {
		return
	}
	l.lastGC = now

	for ip, found := range l.visitors {
		if now.Sub(found.lastSeen) > l.ttl {
			delete(l.visitors, ip)
		}
	}
}

// clientIP devuelve la IP a la que cobrarle el pedido.
//
// X-Forwarded-For se acepta únicamente si la conexión viene de loopback, que es
// el caso de nuestro despliegue: Caddy corre en el mismo host y es el único que
// puede alcanzar al control plane. Desde cualquier otro origen el encabezado se
// ignora, porque lo puede escribir cualquiera.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" && isLoopback(host) {
		// El primero de la lista es el cliente original.
		if comma := strings.Index(forwarded, ","); comma >= 0 {
			forwarded = forwarded[:comma]
		}
		if trimmed := strings.TrimSpace(forwarded); trimmed != "" {
			return trimmed
		}
	}

	return host
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
