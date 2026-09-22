package api

import (
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/metrics"
)

// counterCache recuerda los últimos contadores de red de cada servidor, para
// poder mandarle al panel tasas en vivo sin volver a leer la base.
//
// Es memoria y nada más: si el control plane se reinicia, la primera muestra de
// cada servidor va sin tasas y la siguiente ya las tiene. Ocupa unas decenas de
// bytes por interfaz.
type counterCache struct {
	mu      sync.Mutex
	servers map[uuid.UUID]map[string]counterAt
}

type counterAt struct {
	ts     time.Time
	rx, tx uint64
}

func newCounterCache() *counterCache {
	return &counterCache{servers: make(map[uuid.UUID]map[string]counterAt)}
}

// rates devuelve las tasas de la muestra contra la anterior del mismo servidor,
// y se queda con los contadores nuevos.
//
// Se descarta lo que no es una tasa, con la misma regla del agregado por minuto
// (ADR-009): un contador que baja es un reinicio del servidor, y un hueco de
// más de MaxRateGap es un agent que estuvo caído.
func (c *counterCache) rates(serverID uuid.UUID, sample proto.Sample) []networkPoint {
	ts := sample.TS.UTC()

	c.mu.Lock()
	defer c.mu.Unlock()

	previous, ok := c.servers[serverID]
	if !ok {
		previous = make(map[string]counterAt, len(sample.Networks))
	}

	points := make([]networkPoint, 0, len(sample.Networks))
	current := make(map[string]counterAt, len(sample.Networks))
	for _, network := range sample.Networks {
		current[network.Interface] = counterAt{ts: ts, rx: network.RxBytes, tx: network.TxBytes}

		last, seen := previous[network.Interface]
		if !seen {
			continue
		}
		seconds := ts.Sub(last.ts).Seconds()
		if seconds <= 0 || seconds > metrics.MaxRateGap.Seconds() ||
			network.RxBytes < last.rx || network.TxBytes < last.tx {
			continue
		}

		rx := float64(network.RxBytes-last.rx) / seconds
		tx := float64(network.TxBytes-last.tx) / seconds
		points = append(points, networkPoint{
			Interface:   network.Interface,
			TS:          ts,
			RxPerSec:    rx,
			RxMaxPerSec: rx,
			TxPerSec:    tx,
			TxMaxPerSec: tx,
		})
	}

	c.servers[serverID] = current
	return points
}

// forget saca lo guardado de un servidor. Se llama cuando su agent se
// desconecta: lo que quedó es viejo, y la próxima tasa no puede salir de
// comparar contra una muestra de hace horas.
func (c *counterCache) forget(serverID uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.servers, serverID)
}
