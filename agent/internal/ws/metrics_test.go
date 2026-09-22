package ws

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/JuanBaez77/status-sv/agent/internal/metrics"
	"github.com/JuanBaez77/status-sv/proto"
)

// handleMetrics atiende un lote según el modo del control plane de mentira.
func (f *fakeControlPlane) handleMetrics(ctx context.Context, conn *websocket.Conn, envelope proto.Envelope) error {
	batch, err := proto.Decode[proto.Metrics](envelope)
	if err != nil {
		return err
	}

	f.mu.Lock()
	f.samples = append(f.samples, batch.Samples...)
	f.metricsMessages++
	mode := f.metricsMode
	f.mu.Unlock()

	var reply proto.Message
	switch mode {
	case metricsAck:
		reply = proto.MetricsAck{Accepted: len(batch.Samples), LastTS: batch.Samples[len(batch.Samples)-1].TS}
	case metricsReject:
		reply = proto.NewError(proto.CodeInvalidMessage, "este control plane todavía no guarda métricas", envelope.ID)
	case metricsSilent:
		return nil
	}

	raw, err := proto.Marshal(reply)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, raw)
}

func (f *fakeControlPlane) setMetricsMode(mode metricsMode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metricsMode = mode
}

func (f *fakeControlPlane) metricsStats() (samples int, messages int, connections int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.samples), f.metricsMessages, f.connections
}

var sampleTS = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

func testSample(i int) proto.Sample {
	return proto.Sample{
		TS:     sampleTS.Add(time.Duration(i) * 10 * time.Second),
		CPU:    proto.CPUSample{UsedPercent: 1, Cores: 2},
		Memory: proto.MemorySample{TotalBytes: 100, UsedBytes: 50},
	}
}

// bigSample es una muestra de unos 55 KB: con discos de nombres largos, pocas
// entran en un mensaje.
func bigSample(i int) proto.Sample {
	sample := testSample(i)
	sample.Disks = make([]proto.DiskSample, proto.MaxDisksPerSample)
	for d := range sample.Disks {
		sample.Disks[d] = proto.DiskSample{
			Mountpoint: fmt.Sprintf("/%s%03d", strings.Repeat("<", proto.MaxDeviceNameLen-4), d),
			TotalBytes: 1 << 40,
			UsedBytes:  1 << 39,
		}
	}
	return sample
}

func newMetricsTestClient(t *testing.T, fake *fakeControlPlane, samples ...proto.Sample) (*Client, *metrics.Buffer) {
	t.Helper()
	client := newTestClient(t, fake)
	for _, s := range samples {
		client.buffer.Add(s)
	}
	return client, client.buffer
}

func runClient(t *testing.T, client *Client) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = client.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("se agotó el tiempo esperando: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestClientSendsMetricsAndReleasesThemOnAck(t *testing.T) {
	fake := newFakeControlPlane(t)
	client, buffer := newMetricsTestClient(t, fake, testSample(0), testSample(1), testSample(2))
	runClient(t, client)

	eventually(t, "que el buffer quede vacío", func() bool { return buffer.Len() == 0 })

	samples, messages, _ := fake.metricsStats()
	if samples != 3 || messages != 1 {
		t.Errorf("llegaron %d muestras en %d mensajes, se esperaban 3 en uno", samples, messages)
	}
}

// Es lo que va a pasar contra el control plane actual, que todavía no guarda
// métricas: el lote se descarta y el canal sigue vivo para el latido.
func TestClientDiscardsMetricsTheControlPlaneRejects(t *testing.T) {
	fake := newFakeControlPlane(t)
	fake.setMetricsMode(metricsReject)
	client, buffer := newMetricsTestClient(t, fake, testSample(0), testSample(1))
	runClient(t, client)

	eventually(t, "que se descarten las muestras rechazadas", func() bool { return buffer.Dropped() == 2 })

	if buffer.Len() != 0 {
		t.Errorf("quedaron %d muestras rechazadas en el buffer", buffer.Len())
	}
	if _, _, connections := fake.metricsStats(); connections != 1 {
		t.Errorf("el rechazo cortó el canal: hubo %d conexiones", connections)
	}
}

// Lo que no se confirmó antes de un corte sale de nuevo al reconectar.
func TestClientResendsUnconfirmedMetricsAfterReconnecting(t *testing.T) {
	fake := newFakeControlPlane(t)
	fake.setMetricsMode(metricsSilent)
	client, buffer := newMetricsTestClient(t, fake, testSample(0), testSample(1))
	runClient(t, client)

	eventually(t, "el primer envío", func() bool {
		_, messages, _ := fake.metricsStats()
		return messages == 1
	})
	if buffer.Len() != 2 {
		t.Fatalf("se soltaron muestras sin acuse: quedan %d", buffer.Len())
	}

	fake.setMetricsMode(metricsAck)
	fake.dropConnections()

	eventually(t, "que el buffer quede vacío tras reconectar", func() bool { return buffer.Len() == 0 })
	if samples, _, connections := fake.metricsStats(); samples != 4 || connections < 2 {
		t.Errorf("llegaron %d muestras en %d conexiones, se esperaban las 2 dos veces", samples, connections)
	}
}

// Un buffer lleno de muestras grandes sale en varios mensajes, todos por
// debajo del tope: si alguno lo pasara, el control plane cortaría el canal y
// el agent entraría en un bucle de reconexiones sin vaciar nunca el buffer.
func TestClientSplitsBigBacklogsUnderTheMessageLimit(t *testing.T) {
	const count = 20
	samples := make([]proto.Sample, count)
	for i := range samples {
		samples[i] = bigSample(i)
	}

	fake := newFakeControlPlane(t)
	client, buffer := newMetricsTestClient(t, fake, samples...)
	runClient(t, client)

	eventually(t, "que el buffer quede vacío", func() bool { return buffer.Len() == 0 })

	got, messages, connections := fake.metricsStats()
	if got != count {
		t.Errorf("llegaron %d muestras, se esperaban %d", got, count)
	}
	if messages < 2 {
		t.Errorf("salió todo en %d mensaje: la prueba no está forzando el tope", messages)
	}
	if connections != 1 {
		t.Errorf("hubo %d conexiones: algún mensaje pasó el tope y cortó el canal", connections)
	}
}

func TestEncodeMetrics(t *testing.T) {
	small := []proto.Sample{testSample(0), testSample(1), testSample(2)}
	raw, _, n, err := encodeMetrics(small)
	if err != nil {
		t.Fatalf("encodeMetrics: %v", err)
	}
	if n != len(small) {
		t.Errorf("un lote chico se partió: salieron %d de %d", n, len(small))
	}
	if _, err := proto.ParseEnvelope(raw); err != nil {
		t.Errorf("el mensaje armado no se puede leer: %v", err)
	}

	big := make([]proto.Sample, 10)
	for i := range big {
		big[i] = bigSample(i)
	}
	raw, _, n, err = encodeMetrics(big)
	if err != nil {
		t.Fatalf("encodeMetrics: %v", err)
	}
	if n == 0 || n == len(big) || len(raw) > proto.MaxMessageBytes {
		t.Errorf("salieron %d de %d muestras en %d bytes (tope %d)", n, len(big), len(raw), proto.MaxMessageBytes)
	}
}
