package api

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestIPLimiterAllowsBurstThenBlocks(t *testing.T) {
	limiter := newIPLimiter(60, 3, limiterTTL) // 1 por segundo, ráfaga de 3

	for i := range 3 {
		if !limiter.allow("10.0.0.1") {
			t.Fatalf("el pedido %d de la ráfaga fue rechazado", i+1)
		}
	}
	if limiter.allow("10.0.0.1") {
		t.Fatal("el cuarto pedido inmediato debería haberse rechazado")
	}

	// Otra IP arranca con su propia ráfaga.
	if !limiter.allow("10.0.0.2") {
		t.Fatal("una IP distinta no debería verse afectada")
	}
}

func TestIPLimiterRecoversOverTime(t *testing.T) {
	limiter := newIPLimiter(60, 1, limiterTTL) // 1 por segundo

	if !limiter.allow("10.0.0.1") {
		t.Fatal("el primer pedido fue rechazado")
	}
	if limiter.allow("10.0.0.1") {
		t.Fatal("el segundo pedido inmediato debería haberse rechazado")
	}

	// rate.Limiter usa el reloj real, así que se espera lo mínimo indispensable.
	time.Sleep(1100 * time.Millisecond)

	if !limiter.allow("10.0.0.1") {
		t.Fatal("después de esperar, el pedido debería pasar")
	}
}

func TestIPLimiterForgetsInactiveIPs(t *testing.T) {
	limiter := newIPLimiter(60, 1, time.Minute)

	now := time.Now()
	limiter.now = func() time.Time { return now }

	limiter.allow("10.0.0.1")
	if len(limiter.visitors) != 1 {
		t.Fatalf("se esperaba 1 visitante, hay %d", len(limiter.visitors))
	}

	// Pasa el tiempo suficiente como para olvidarla, y llega otra IP.
	now = now.Add(2 * time.Minute)
	limiter.allow("10.0.0.2")

	if _, existe := limiter.visitors["10.0.0.1"]; existe {
		t.Error("la IP inactiva debería haberse olvidado")
	}
	if len(limiter.visitors) != 1 {
		t.Errorf("quedaron %d visitantes, se esperaba 1", len(limiter.visitors))
	}
}

func TestBearerCredential(t *testing.T) {
	tests := []struct {
		name   string
		header string
		wantOK bool
		want   string
	}{
		{name: "sin encabezado", wantOK: false},
		{name: "credencial normal", header: "Bearer svagt_abc123", wantOK: true, want: "svagt_abc123"},
		{name: "prefijo en minúsculas", header: "bearer svagt_abc123", wantOK: true, want: "svagt_abc123"},
		{name: "con espacios de más", header: "Bearer   svagt_abc123  ", wantOK: true, want: "svagt_abc123"},
		{name: "otro esquema", header: "Basic dXNlcjpwYXNz", wantOK: false},
		{name: "sin valor", header: "Bearer ", wantOK: false},
		{name: "solo el esquema", header: "Bearer", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/agents/connect", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}

			secret, ok := bearerCredential(req)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, se esperaba %v", ok, tt.wantOK)
			}
			if ok && secret.Reveal() != tt.want {
				t.Errorf("credencial = %q, se esperaba %q", secret.Reveal(), tt.want)
			}
		})
	}
}

// X-Forwarded-For solo vale si el pedido viene de un proxy de confianza: si no,
// cualquiera podría poner la IP que quiera y saltarse el límite de frecuencia.
func TestClientIP(t *testing.T) {
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	// La red interna de Docker donde corre Caddy en el VPS.
	dockerNet := []netip.Prefix{netip.MustParsePrefix("172.30.0.0/24")}

	tests := []struct {
		name       string
		trusted    []netip.Prefix
		remoteAddr string
		forwarded  string
		want       string
	}{
		{
			name:       "sin proxy",
			trusted:    loopback,
			remoteAddr: "203.0.113.7:54321",
			want:       "203.0.113.7",
		},
		{
			name:       "detrás del proxy local",
			trusted:    loopback,
			remoteAddr: "127.0.0.1:44321",
			forwarded:  "203.0.113.7",
			want:       "203.0.113.7",
		},
		{
			name:       "cadena de proxies",
			trusted:    loopback,
			remoteAddr: "127.0.0.1:44321",
			forwarded:  "203.0.113.7, 198.51.100.2",
			want:       "203.0.113.7",
		},
		{
			name:       "encabezado falsificado desde afuera",
			trusted:    loopback,
			remoteAddr: "203.0.113.7:54321",
			forwarded:  "10.0.0.1",
			want:       "203.0.113.7",
		},
		{
			name:       "encabezado vacío desde el proxy",
			trusted:    loopback,
			remoteAddr: "127.0.0.1:44321",
			forwarded:  "   ",
			want:       "127.0.0.1",
		},
		{
			name:       "IPv4 escrita como IPv6",
			trusted:    loopback,
			remoteAddr: "[::ffff:127.0.0.1]:44321",
			forwarded:  "203.0.113.7",
			want:       "203.0.113.7",
		},
		// La topología del VPS: Caddy en otro contenedor, en la red interna.
		{
			name:       "Caddy desde la red de Docker",
			trusted:    dockerNet,
			remoteAddr: "172.30.0.5:40000",
			forwarded:  "203.0.113.7",
			want:       "203.0.113.7",
		},
		{
			name:       "otro contenedor fuera de la red de confianza",
			trusted:    dockerNet,
			remoteAddr: "172.31.0.9:40000",
			forwarded:  "203.0.113.7",
			want:       "172.31.0.9",
		},
		{
			name:       "con la red de Docker configurada, loopback deja de ser de confianza",
			trusted:    dockerNet,
			remoteAddr: "127.0.0.1:44321",
			forwarded:  "203.0.113.7",
			want:       "127.0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/agents/enroll", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.forwarded != "" {
				req.Header.Set("X-Forwarded-For", tt.forwarded)
			}

			if got := clientIP(req, tt.trusted); got != tt.want {
				t.Errorf("clientIP() = %q, se esperaba %q", got, tt.want)
			}
		})
	}
}
