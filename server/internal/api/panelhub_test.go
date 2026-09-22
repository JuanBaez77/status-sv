package api

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

func TestPanelHubOnlySendsToSubscribers(t *testing.T) {
	hub := newPanelHub()
	first, second := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())

	watcher := newPanelClient()
	hub.add(watcher)
	watcher.subscribe([]uuid.UUID{first})

	other := newPanelClient()
	hub.add(other)
	other.subscribe([]uuid.UUID{second})

	if !hub.hasSubscribers(first) || hub.hasSubscribers(uuid.Must(uuid.NewV7())) {
		t.Error("hasSubscribers no distingue los servidores mirados")
	}
	if sent := hub.publish(first, []byte(`{"type":"sample"}`)); sent != 1 {
		t.Errorf("el mensaje llegó a %d paneles, se esperaba 1", sent)
	}
	if len(watcher.send) != 1 || len(other.send) != 0 {
		t.Errorf("colas = %d y %d; solo el suscripto tenía que recibir", len(watcher.send), len(other.send))
	}

	// Al desuscribirse deja de recibir.
	watcher.unsubscribe([]uuid.UUID{first})
	if sent := hub.publish(first, []byte(`{}`)); sent != 0 {
		t.Errorf("después de desuscribirse recibió %d mensajes", sent)
	}
}

func TestPanelClientDropsWhenItDoesNotRead(t *testing.T) {
	client := newPanelClient()
	server := uuid.Must(uuid.NewV7())
	client.subscribe([]uuid.UUID{server})

	for range sendBuffer {
		if !client.offer([]byte(`{}`)) {
			t.Fatal("el buffer se llenó antes de tiempo")
		}
	}
	// El que no entra se descarta: el panel se pone al día con la API REST.
	if client.offer([]byte(`{}`)) {
		t.Error("se aceptó un mensaje de más")
	}
	if client.droppedCount() != 1 {
		t.Errorf("descartados = %d, se esperaba 1", client.droppedCount())
	}
}

func TestPanelClientLimitsSubscriptions(t *testing.T) {
	client := newPanelClient()
	ids := make([]uuid.UUID, maxSubscriptions+10)
	for i := range ids {
		ids[i] = uuid.Must(uuid.NewV7())
	}

	client.subscribe(ids)
	if got := len(client.servers); got != maxSubscriptions {
		t.Errorf("%d suscripciones, el tope es %d", got, maxSubscriptions)
	}
}

func TestPanelClientCloseIsIdempotent(t *testing.T) {
	client := newPanelClient()
	client.close()
	client.close() // no tiene que entrar en pánico por cerrar dos veces
	select {
	case <-client.done:
	default:
		t.Error("close no avisó al escritor")
	}
}

func TestCounterCacheRates(t *testing.T) {
	cache := newCounterCache()
	server := uuid.Must(uuid.NewV7())
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	sample := func(ts time.Time, rx, tx uint64) proto.Sample {
		return proto.Sample{TS: ts, Networks: []proto.NetworkSample{{Interface: "eth0", RxBytes: rx, TxBytes: tx}}}
	}

	// La primera muestra no tiene con qué compararse.
	if got := cache.rates(server, sample(at, 1000, 500)); len(got) != 0 {
		t.Errorf("la primera muestra dio tasas: %+v", got)
	}

	got := cache.rates(server, sample(at.Add(10*time.Second), 3000, 1000))
	if len(got) != 1 || got[0].RxPerSec != 200 || got[0].TxPerSec != 50 {
		t.Fatalf("tasas = %+v; se esperaba 200 y 50 por segundo", got)
	}
	if got[0].Interface != "eth0" || !got[0].TS.Equal(at.Add(10*time.Second)) {
		t.Errorf("punto = %+v", got[0])
	}

	// Un contador que baja es un reinicio del servidor.
	if got := cache.rates(server, sample(at.Add(20*time.Second), 10, 10)); len(got) != 0 {
		t.Errorf("un reinicio del contador dio tasas: %+v", got)
	}
	// Un hueco largo tampoco es una tasa.
	if got := cache.rates(server, sample(at.Add(time.Hour), 900_000, 900_000)); len(got) != 0 {
		t.Errorf("un hueco de una hora dio tasas: %+v", got)
	}

	// Olvidar al servidor deja la próxima muestra sin con qué comparar.
	cache.rates(server, sample(at.Add(2*time.Hour), 1_000_000, 1_000_000))
	cache.forget(server)
	if got := cache.rates(server, sample(at.Add(2*time.Hour).Add(10*time.Second), 1_100_000, 1_000_000)); len(got) != 0 {
		t.Errorf("después de forget salieron tasas: %+v", got)
	}
}
