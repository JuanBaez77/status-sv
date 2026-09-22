// Package metrics junta las muestras del colector y las guarda hasta que el
// control plane confirma que las guardó.
//
// El muestreo no depende del canal: si la conexión se corta, las muestras se
// siguen tomando y esperan acá, hasta 10 minutos, a que el canal vuelva.
package metrics

import (
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

// Valores del diseño (docs/DISENO.md, "Confiabilidad").
const (
	// DefaultMaxAge es cuánto aguanta el agent sin conexión sin perder muestras.
	DefaultMaxAge = 10 * time.Minute
	// DefaultMaxSamples es el techo por cantidad: 10 minutos con el intervalo
	// más corto que acepta el contrato, de 1 segundo. Con el intervalo normal de
	// 10 segundos manda el límite por antigüedad y quedan 60.
	DefaultMaxSamples = 600
	// DefaultAckTimeout es cuánto se espera el acuse de un lote antes de darlo
	// por perdido y volver a mandarlo.
	DefaultAckTimeout = 30 * time.Second
)

// Buffer guarda las muestras que el control plane todavía no confirmó.
//
// Es una cola con un solo lote en vuelo: se manda un lote, se espera la
// respuesta y recién ahí sale el siguiente. El acuse solo dice hasta qué
// marca de tiempo se guardó, sin decir a qué lote responde, y con un lote a la
// vez no hace falta más. Los lotes salen siempre desde la muestra más vieja.
//
// Es seguro usarlo desde varias goroutines: el muestreo agrega, el envío toma
// lotes y la lectura del canal procesa las respuestas.
type Buffer struct {
	mu sync.Mutex

	entries    []entry // de la más vieja a la más nueva
	nextSeq    uint64
	maxAge     time.Duration
	maxSamples int
	ackTimeout time.Duration

	// inFlight es el lote enviado que todavía no tuvo respuesta, o nil.
	inFlight *flight
	// maxSentSeq es la muestra más nueva que salió alguna vez. Un acuse nunca
	// suelta una muestra posterior: no puede estar guardada si no se mandó.
	maxSentSeq uint64
	anySent    bool

	dropped int
}

// entry es una muestra con su número de orden. El número, y no la marca de
// tiempo, dice qué salió y qué no: el reloj del sistema puede saltar.
type entry struct {
	seq    uint64
	sample proto.Sample
}

type flight struct {
	id      uuid.UUID
	lastSeq uint64
	sentAt  time.Time
}

// Batch es un lote para mandar: las muestras más viejas del buffer, en orden.
type Batch struct {
	Samples  []proto.Sample
	firstSeq uint64
}

// NewBuffer arma un buffer que guarda hasta maxSamples muestras y ninguna más
// vieja que maxAge respecto de la más nueva.
func NewBuffer(maxAge time.Duration, maxSamples int) *Buffer {
	return &Buffer{
		maxAge:     maxAge,
		maxSamples: max(maxSamples, 1),
		ackTimeout: DefaultAckTimeout,
	}
}

// Add agrega una muestra. Si el buffer se pasa de antigüedad o de cantidad,
// descarta las más viejas y devuelve cuántas.
func (b *Buffer) Add(sample proto.Sample) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.entries = append(b.entries, entry{seq: b.nextSeq, sample: sample})
	b.nextSeq++

	oldest := sample.TS.Add(-b.maxAge)
	drop := 0
	for drop < len(b.entries)-1 {
		tooMany := len(b.entries)-drop > b.maxSamples
		tooOld := b.entries[drop].sample.TS.Before(oldest)
		if !tooMany && !tooOld {
			break
		}
		drop++
	}
	b.popFront(drop)
	b.dropped += drop
	return drop
}

// Next devuelve el próximo lote, con hasta limit muestras.
//
// Devuelve un lote vacío si no hay nada o si hay un lote en vuelo que todavía
// puede recibir respuesta. Si la respuesta tardó más que el plazo, el lote se
// da por perdido y sus muestras vuelven a salir: el control plane guarda de
// forma idempotente, así que un duplicado no hace daño y una pérdida sí.
func (b *Buffer) Next(limit int, now time.Time) Batch {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.inFlight != nil {
		if now.Sub(b.inFlight.sentAt) < b.ackTimeout {
			return Batch{}
		}
		b.inFlight = nil
	}

	n := min(len(b.entries), limit)
	if n <= 0 {
		return Batch{}
	}

	batch := Batch{Samples: make([]proto.Sample, n), firstSeq: b.entries[0].seq}
	for i := range n {
		batch.Samples[i] = b.entries[i].sample
	}
	return batch
}

// Sent registra que salieron las primeras n muestras del lote, en el mensaje
// con ese ID. Puede ser menos que el lote entero si no entraba en un mensaje.
func (b *Buffer) Sent(batch Batch, n int, id uuid.UUID, now time.Time) {
	if n <= 0 || n > len(batch.Samples) {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	lastSeq := batch.firstSeq + uint64(n) - 1
	b.inFlight = &flight{id: id, lastSeq: lastSeq, sentAt: now}
	if !b.anySent || lastSeq > b.maxSentSeq {
		b.maxSentSeq, b.anySent = lastSeq, true
	}
}

// Ack procesa un metrics_ack: suelta las muestras que ya salieron y tienen
// una marca de tiempo hasta lastTS, y habilita el lote siguiente. Lo que haya
// quedado del lote sin confirmar vuelve a salir en el próximo. Devuelve cuántas
// muestras soltó.
func (b *Buffer) Ack(lastTS time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.inFlight = nil
	if !b.anySent {
		return 0
	}

	release := 0
	for release < len(b.entries) {
		e := b.entries[release]
		if e.seq > b.maxSentSeq || e.sample.TS.After(lastTS) {
			break
		}
		release++
	}
	b.popFront(release)
	return release
}

// Reject procesa un error del control plane que responde al mensaje id.
//
// Si el error no es del lote en vuelo, devuelve false y no toca nada. Si lo es
// y se puede reintentar (el control plane está saturado o tuvo un problema
// interno), las muestras vuelven a salir en el próximo lote. Si no se puede,
// el control plane no las va a aceptar nunca: se descartan para no trabar el
// buffer, y devuelve cuántas.
func (b *Buffer) Reject(id uuid.UUID, retryable bool) (discarded int, matched bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.inFlight == nil || b.inFlight.id != id {
		return 0, false
	}
	flight := b.inFlight
	b.inFlight = nil
	if retryable {
		return 0, true
	}

	for discarded < len(b.entries) && b.entries[discarded].seq <= flight.lastSeq {
		discarded++
	}
	b.popFront(discarded)
	b.dropped += discarded
	return discarded, true
}

// Restart se llama al abrir un canal nuevo: la respuesta al lote que estaba en
// vuelo ya no va a llegar, así que sus muestras vuelven a salir.
func (b *Buffer) Restart() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inFlight = nil
}

// DiscardOldest descarta las n muestras más viejas. Es una salida de emergencia
// para una muestra que no se puede mandar, que si no trabaría la cola.
func (b *Buffer) DiscardOldest(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n = min(n, len(b.entries))
	b.popFront(n)
	b.dropped += n
}

// Len devuelve cuántas muestras esperan confirmación.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

// Dropped devuelve cuántas muestras se perdieron desde que arrancó el agent,
// por falta de lugar o porque el control plane las rechazó.
func (b *Buffer) Dropped() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// popFront saca las n primeras. Copia en vez de recortar el slice para que la
// memoria del buffer no crezca con el tiempo.
func (b *Buffer) popFront(n int) {
	if n > 0 {
		b.entries = slices.Delete(b.entries, 0, n)
	}
}
