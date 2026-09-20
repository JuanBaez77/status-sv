package ws

import (
	"math"
	"math/rand/v2"
	"time"
)

// Valores por defecto del backoff. El tope de un minuto es un equilibrio: si el
// control plane estuvo caído una hora, el agent vuelve dentro del minuto de que
// se recupere, sin castigarlo con reintentos cada segundo mientras tanto.
const (
	DefaultBaseDelay = time.Second
	DefaultMaxDelay  = time.Minute
	// maxExponent evita que el desplazamiento se desborde en corridas largas.
	maxExponent = 20
)

// Backoff calcula cuánto esperar antes de reintentar una conexión.
//
// La espera crece al doble en cada intento y lleva jitter: queda entre la mitad
// y el total del tope exponencial. El jitter importa cuando hay varios agents,
// porque si el control plane se reinicia todos se caen a la vez y, sin
// dispersión, volverían todos juntos en el mismo instante.
type Backoff struct {
	// Base es la espera del primer reintento.
	Base time.Duration
	// Max es el techo de la espera.
	Max time.Duration
	// random devuelve un número en [0,1). Los tests lo reemplazan.
	random func() float64

	attempts int
}

// NewBackoff arma un backoff con los valores por defecto.
func NewBackoff() *Backoff {
	return &Backoff{Base: DefaultBaseDelay, Max: DefaultMaxDelay}
}

// Next devuelve la espera del próximo reintento y avanza el contador.
func (b *Backoff) Next() time.Duration {
	base := b.Base
	if base <= 0 {
		base = DefaultBaseDelay
	}
	maximum := b.Max
	if maximum <= 0 {
		maximum = DefaultMaxDelay
	}

	exponent := min(b.attempts, maxExponent)
	b.attempts++

	ceiling := time.Duration(float64(base) * math.Pow(2, float64(exponent)))
	if ceiling > maximum || ceiling <= 0 { // <= 0 cubre el desborde
		ceiling = maximum
	}

	// Jitter a la mitad: la espera queda entre ceiling/2 y ceiling.
	half := ceiling / 2
	return half + time.Duration(b.rand()*float64(half))
}

// Attempts dice cuántos reintentos van desde el último Reset.
func (b *Backoff) Attempts() int {
	return b.attempts
}

// Reset vuelve el backoff al principio. Se llama cuando una conexión funcionó,
// para que la próxima caída no herede la espera larga de la anterior.
func (b *Backoff) Reset() {
	b.attempts = 0
}

func (b *Backoff) rand() float64 {
	if b.random != nil {
		return b.random()
	}
	return rand.Float64() //nolint:gosec // dispersión de reintentos, no criptografía
}
