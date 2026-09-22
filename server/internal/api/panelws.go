package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

// Mensajes del panel hacia el control plane.
const (
	panelSubscribe   = "subscribe"
	panelUnsubscribe = "unsubscribe"
)

// Mensajes del control plane hacia el panel.
const (
	panelWelcome    = "welcome"
	panelSubscribed = "subscribed"
	panelSample     = "sample"
	panelError      = "error"
)

// Límites del canal del panel.
const (
	// panelReadLimit acota lo que manda el panel: sus mensajes son una lista de
	// IDs, nada más.
	panelReadLimit = 64 << 10
	// panelPingEvery mantiene viva la conexión a través de proxies que cortan
	// las conexiones ociosas.
	panelPingEvery = 30 * time.Second
	// panelPingTimeout es cuánto se espera el pong antes de dar la conexión por
	// muerta.
	panelPingTimeout = 10 * time.Second
)

type panelRequest struct {
	Type      string      `json:"type"`
	ServerIDs []uuid.UUID `json:"server_ids"`
}

type panelWelcomeMessage struct {
	Type                string `json:"type"`
	OfflineAfterSeconds int    `json:"offline_after_seconds"`
}

// panelSubscribedMessage confirma a qué servidores quedó suscripto el panel.
// Sirve para que el front sepa que ya puede esperar muestras.
type panelSubscribedMessage struct {
	Type      string      `json:"type"`
	ServerIDs []uuid.UUID `json:"server_ids"`
}

type panelErrorMessage struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// panelSampleMessage es una muestra recién guardada, con la misma forma que los
// puntos de la API REST: el panel usa el mismo código para dibujarla.
type panelSampleMessage struct {
	Type     string         `json:"type"`
	ServerID uuid.UUID      `json:"server_id"`
	Sample   samplePoint    `json:"sample"`
	Disks    []diskPoint    `json:"disks"`
	Networks []networkPoint `json:"networks"`
}

// handlePanelConnect abre el canal por el que el panel recibe las métricas en
// vivo. Se autentica con la misma cookie de sesión que el resto del panel
// (requireSession lo envuelve), así que acá la sesión ya está verificada.
//
// El panel manda a qué servidores quiere seguir; sin suscripción no recibe
// nada. Así una pestaña abierta en un servidor no paga por los demás.
func (s *Server) handlePanelConnect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	session := SessionFrom(ctx)

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.WarnContext(ctx, "no se pudo abrir el WebSocket del panel", "usuario", session.Username, "error", err)
		return
	}
	conn.SetReadLimit(panelReadLimit)
	defer closeQuietly(conn, websocket.StatusInternalError, "cerrando")

	client := newPanelClient()
	id := s.panels.add(client)
	defer s.panels.remove(id)
	defer client.close()

	s.log.InfoContext(ctx, "panel conectado", "usuario", session.Username, "paneles", s.panels.count())

	// El escritor es el único que escribe en la conexión: el WebSocket no
	// admite dos escrituras a la vez.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Go(func() {
		defer cancel()
		s.panelWriteLoop(ctx, conn, client)
	})

	err = s.panelReadLoop(ctx, conn, client)
	cancel()
	wg.Wait()

	switch {
	case errors.Is(err, context.Canceled),
		websocket.CloseStatus(err) == websocket.StatusNormalClosure,
		websocket.CloseStatus(err) == websocket.StatusGoingAway:
		s.log.InfoContext(ctx, "panel desconectado", "usuario", session.Username,
			"mensajes_descartados", client.droppedCount())
	default:
		s.log.InfoContext(ctx, "panel desconectado con error", "usuario", session.Username, "error", err)
	}
}

// panelReadLoop atiende las suscripciones que manda el panel.
func (s *Server) panelReadLoop(ctx context.Context, conn *websocket.Conn, client *panelClient) error {
	welcome, err := json.Marshal(panelWelcomeMessage{
		Type:                panelWelcome,
		OfflineAfterSeconds: int(s.cfg.OfflineAfter.Seconds()),
	})
	if err != nil {
		return err
	}
	client.offer(welcome)

	for {
		messageType, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if messageType != websocket.MessageText {
			client.offer(panelError2JSON("los mensajes son JSON de texto"))
			continue
		}

		var request panelRequest
		if err := json.Unmarshal(data, &request); err != nil {
			client.offer(panelError2JSON("no se pudo leer el mensaje"))
			continue
		}

		switch request.Type {
		case panelSubscribe:
			client.subscribe(request.ServerIDs)
			client.offer(subscribedJSON(client))
		case panelUnsubscribe:
			client.unsubscribe(request.ServerIDs)
			client.offer(subscribedJSON(client))
		default:
			client.offer(panelError2JSON("tipo de mensaje desconocido: " + request.Type))
		}
	}
}

// panelWriteLoop manda lo que se va publicando, y un ping cada tanto para que
// un proxy no corte la conexión por estar ociosa.
func (s *Server) panelWriteLoop(ctx context.Context, conn *websocket.Conn, client *panelClient) {
	ticker := time.NewTicker(panelPingEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-client.done:
			closeQuietly(conn, websocket.StatusGoingAway, "el control plane se está apagando")
			return
		case message := <-client.send:
			writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := conn.Write(writeCtx, websocket.MessageText, message)
			cancel()
			if err != nil {
				return
			}
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, panelPingTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func subscribedJSON(client *panelClient) []byte {
	raw, err := json.Marshal(panelSubscribedMessage{Type: panelSubscribed, ServerIDs: client.list()})
	if err != nil {
		return panelError2JSON("no se pudo confirmar la suscripción")
	}
	return raw
}

// clampBytes pasa un contador sin signo al entero con signo que usan las
// respuestas. La ingesta ya rechazó lo que no entra en bigint, así que esto es
// una red de seguridad: el panel prefiere un número tapado antes que uno
// negativo.
func clampBytes(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

func panelError2JSON(message string) []byte {
	raw, err := json.Marshal(panelErrorMessage{Type: panelError, Message: message})
	if err != nil {
		// El mensaje lo arma este paquete: si no se puede serializar, es un bug.
		return []byte(`{"type":"error","message":"error interno"}`)
	}
	return raw
}

// publishSample les manda a los paneles suscriptos la muestra más nueva de un
// lote recién guardado.
//
// Si no hay nadie mirando ese servidor no se arma nada. Las tasas de red salen
// de comparar con la muestra anterior del mismo canal; la primera muestra
// después de conectar no tiene con qué compararse y va sin red.
func (s *Server) publishSample(serverID uuid.UUID, samples []proto.Sample) {
	if len(samples) == 0 || !s.panels.hasSubscribers(serverID) {
		return
	}

	// El lote puede traer varias muestras: se recorren en orden para que los
	// contadores de red queden al día, y se publica la más nueva.
	ordered := slices.Clone(samples)
	slices.SortFunc(ordered, func(a, b proto.Sample) int { return a.TS.Compare(b.TS) })
	newest := ordered[len(ordered)-1]

	var rates []networkPoint
	for _, sample := range ordered {
		rates = s.counters.rates(serverID, sample)
	}

	message := panelSampleMessage{
		Type:     panelSample,
		ServerID: serverID,
		Sample: samplePoint{
			TS:                  newest.TS.UTC(),
			CPUUsedPercent:      float32(newest.CPU.UsedPercent),
			CPUUsedMaxPercent:   float32(newest.CPU.UsedPercent),
			CPUIOWaitPercent:    float32(newest.CPU.IOWaitPercent),
			CPUIOWaitMaxPercent: float32(newest.CPU.IOWaitPercent),
			CPUStealPercent:     float32(newest.CPU.StealPercent),
			CPUCores:            int16(newest.CPU.Cores), //nolint:gosec // el contrato lo acota
			MemTotalBytes:       clampBytes(newest.Memory.TotalBytes),
			MemUsedBytes:        clampBytes(newest.Memory.UsedBytes),
			MemUsedMaxBytes:     clampBytes(newest.Memory.UsedBytes),
			SwapTotalBytes:      clampBytes(newest.Memory.SwapTotalBytes),
			SwapUsedBytes:       clampBytes(newest.Memory.SwapUsedBytes),
			SwapUsedMaxBytes:    clampBytes(newest.Memory.SwapUsedBytes),
			Load1:               float32(newest.Load.Load1),
			Load5:               float32(newest.Load.Load5),
			Load15:              float32(newest.Load.Load15),
		},
		Disks:    make([]diskPoint, 0, len(newest.Disks)),
		Networks: []networkPoint{},
	}
	for _, disk := range newest.Disks {
		message.Disks = append(message.Disks, diskPoint{
			TS:             newest.TS.UTC(),
			Mountpoint:     disk.Mountpoint,
			TotalBytes:     clampBytes(disk.TotalBytes),
			UsedBytes:      clampBytes(disk.UsedBytes),
			AvailableBytes: clampBytes(disk.AvailableBytes),
		})
	}
	message.Networks = rates

	raw, err := json.Marshal(message)
	if err != nil {
		s.log.Error("no se pudo armar la muestra para el panel", "error", err)
		return
	}
	s.panels.publish(serverID, raw)
}
