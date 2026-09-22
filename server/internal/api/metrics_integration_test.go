//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/metrics"
)

// connectForMetrics enrola un agent, abre su canal y deja listas las
// particiones de hoy, sin las cuales no se puede guardar nada.
func connectForMetrics(t *testing.T, f *fixture, name, ip string) (*websocket.Conn, uuid.UUID) {
	t.Helper()

	partitions := metrics.NewPartitions(f.queries, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := partitions.Maintain(t.Context()); err != nil {
		t.Fatalf("crear las particiones: %v", err)
	}

	ack := f.enrollOK(t, name, ip)
	conn, _, err := f.dial(t, ack.Credential)
	if err != nil {
		t.Fatalf("abrir el canal: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return conn, ack.ServerID
}

// sendEnvelope manda un mensaje y devuelve su ID, para poder reconocer la
// respuesta.
func sendEnvelope(t *testing.T, conn *websocket.Conn, message proto.Message) uuid.UUID {
	t.Helper()

	envelope, err := proto.Encode(message)
	if err != nil {
		t.Fatalf("armar %s: %v", message.Type(), err)
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("serializar %s: %v", message.Type(), err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("enviar %s: %v", message.Type(), err)
	}
	return envelope.ID
}

func testSample(ts time.Time) proto.Sample {
	return proto.Sample{
		TS:     ts,
		CPU:    proto.CPUSample{UsedPercent: 10, IOWaitPercent: 0.5, StealPercent: 0.25, Cores: 2},
		Memory: proto.MemorySample{TotalBytes: 1 << 30, UsedBytes: 1 << 29},
		Load:   proto.LoadSample{Load1: 0.1},
		Disks:  []proto.DiskSample{{Mountpoint: "/", TotalBytes: 100 << 30, UsedBytes: 40 << 30, AvailableBytes: 55 << 30}},
		Networks: []proto.NetworkSample{
			{Interface: "eth0", RxBytes: 1 << 20, TxBytes: 1 << 19},
		},
	}
}

// El camino que hace el agent cada 10 segundos: manda un lote, se guarda y
// llega el acuse con la marca de la última muestra.
func TestMetricsAreStoredAndAcknowledged(t *testing.T) {
	f := setup(t)
	conn, serverID := connectForMetrics(t, f, "vps-01", "203.0.113.40")

	now := time.Now().UTC().Truncate(time.Millisecond)
	batch := proto.Metrics{Samples: []proto.Sample{testSample(now.Add(-10 * time.Second)), testSample(now)}}

	for attempt := range 2 {
		// El segundo envío es el reenvío de un lote cuyo acuse se perdió.
		sendEnvelope(t, conn, batch)
		envelope := receive(t, conn)
		if envelope.Type != proto.TypeMetricsAck {
			t.Fatalf("envío %d: llegó %q, se esperaba metrics_ack", attempt+1, envelope.Type)
		}
		ack, err := proto.Decode[proto.MetricsAck](envelope)
		if err != nil {
			t.Fatalf("leer el acuse: %v", err)
		}
		if ack.Accepted != 2 || !ack.LastTS.Equal(now) {
			t.Errorf("envío %d: acuse %+v, se esperaban 2 muestras hasta %v", attempt+1, ack, now)
		}
	}

	var samples, disks, networks int
	if err := f.pool.QueryRow(t.Context(), `
		SELECT (SELECT count(*) FROM metrics_raw WHERE server_id = $1),
		       (SELECT count(*) FROM metrics_raw_disks WHERE server_id = $1),
		       (SELECT count(*) FROM metrics_raw_networks WHERE server_id = $1)`, serverID,
	).Scan(&samples, &disks, &networks); err != nil {
		t.Fatalf("contar: %v", err)
	}
	if samples != 2 || disks != 2 || networks != 2 {
		t.Errorf("quedaron %d muestras, %d discos y %d interfaces; se esperaban 2 de cada una", samples, disks, networks)
	}
}

// Un reloj corrido no se arregla reenviando: el lote se rechaza como inválido,
// para que el agent lo descarte, y el canal sigue vivo.
func TestMetricsFromTheFutureAreRejected(t *testing.T) {
	f := setup(t)
	conn, _ := connectForMetrics(t, f, "vps-01", "203.0.113.41")

	id := sendEnvelope(t, conn, proto.Metrics{Samples: []proto.Sample{testSample(time.Now().UTC().Add(time.Hour))}})

	envelope := receive(t, conn)
	if envelope.Type != proto.TypeError {
		t.Fatalf("llegó %q, se esperaba un error", envelope.Type)
	}
	message, err := proto.Decode[proto.ErrorMessage](envelope)
	if err != nil {
		t.Fatalf("leer el error: %v", err)
	}
	if message.Code != proto.CodeInvalidMessage || message.Retryable() {
		t.Errorf("código %q, se esperaba invalid_message (definitivo)", message.Code)
	}
	if message.InReplyTo == nil || *message.InReplyTo != id {
		t.Errorf("el error no dice a qué lote responde: %v", message.InReplyTo)
	}

	send(t, conn, proto.Heartbeat{AgentVersion: "0.1.0"})
	if envelope := receive(t, conn); envelope.Type != proto.TypeHeartbeatAck {
		t.Errorf("el canal no siguió funcionando: llegó %q", envelope.Type)
	}
}

// Un agent que manda lotes sin parar recibe rate_limited, que es reintentable:
// no pierde las muestras, las vuelve a mandar más tarde.
func TestMetricsAreRateLimited(t *testing.T) {
	f := setup(t)
	conn, _ := connectForMetrics(t, f, "vps-01", "203.0.113.42")

	batch := proto.Metrics{Samples: []proto.Sample{testSample(time.Now().UTC().Truncate(time.Millisecond))}}
	const sent = 45

	limited := 0
	for range sent {
		sendEnvelope(t, conn, batch)
		envelope := receive(t, conn)
		if envelope.Type == proto.TypeError {
			message, err := proto.Decode[proto.ErrorMessage](envelope)
			if err != nil {
				t.Fatalf("leer el error: %v", err)
			}
			if message.Code != proto.CodeRateLimited || !message.Retryable() {
				t.Fatalf("código %q, se esperaba rate_limited", message.Code)
			}
			limited++
		}
	}
	if limited == 0 {
		t.Errorf("se aceptaron los %d lotes seguidos sin límite", sent)
	}
}
