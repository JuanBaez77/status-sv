package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/enrollment"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// writeTimeout limita lo que puede tardar una respuesta al agent.
const writeTimeout = 5 * time.Second

// handleAgentConnect abre el canal permanente con un agent ya enrolado.
//
// El agent se autentica con su credencial en el encabezado Authorization; nunca
// en la URL, que quedaría en los logs del proxy (ADR-006).
func (s *Server) handleAgentConnect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	credential, ok := bearerCredential(r)
	if !ok {
		s.writeProtoError(ctx, w, http.StatusUnauthorized, uuid.Nil,
			proto.CodeUnauthorized, "falta la credencial del agent")
		return
	}

	server, err := s.enrollment.Authenticate(ctx, credential)
	if err != nil {
		if errors.Is(err, enrollment.ErrUnauthorized) {
			s.log.WarnContext(ctx, "conexión de agent rechazada", "ip", s.clientIP(r))
			s.writeProtoError(ctx, w, http.StatusUnauthorized, uuid.Nil,
				proto.CodeUnauthorized, "credencial inválida o revocada")
			return
		}
		s.log.ErrorContext(ctx, "no se pudo verificar la credencial", "error", err)
		s.writeProtoError(ctx, w, http.StatusInternalServerError, uuid.Nil,
			proto.CodeInternal, "no se pudo verificar la credencial")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.WarnContext(ctx, "no se pudo abrir el WebSocket",
			"servidor", server.Name, "error", err)
		return
	}
	// El tope es parte del contrato: el agent arma sus lotes para no pasarlo, y
	// más que esto es un error o un abuso.
	conn.SetReadLimit(proto.MaxMessageBytes)
	defer closeQuietly(conn, websocket.StatusInternalError, "cerrando")

	// Si quedó una conexión vieja de este mismo agent (corte de red, por
	// ejemplo), la nueva la reemplaza: la que está viva es esta.
	if previous := s.hub.add(server.ID, conn); previous != nil {
		s.log.InfoContext(ctx, "reemplazando la conexión anterior del agent", "servidor", server.Name)
		closeQuietly(previous, websocket.StatusPolicyViolation, "reemplazada por una conexión nueva")
	}
	defer s.hub.remove(server.ID, conn)
	// Sin el agent, los contadores guardados envejecen: la próxima tasa tiene
	// que salir de dos muestras nuevas.
	defer s.counters.forget(server.ID)

	s.log.InfoContext(ctx, "agent conectado",
		"servidor", server.Name,
		"servidor_id", server.ID,
		"conectados", s.hub.count(),
	)

	// Cada canal tiene sus propios límites de frecuencia.
	limits := channelLimits{
		metrics: rate.NewLimiter(metricsPerSecond, metricsBurst),
		runtime: rate.NewLimiter(runtimePerSecond, runtimeBurst),
	}
	err = s.readLoop(ctx, conn, server, limits)

	switch {
	case errors.Is(err, context.Canceled),
		websocket.CloseStatus(err) == websocket.StatusNormalClosure,
		websocket.CloseStatus(err) == websocket.StatusGoingAway:
		s.log.InfoContext(ctx, "agent desconectado", "servidor", server.Name)
	default:
		s.log.WarnContext(ctx, "agent desconectado con error",
			"servidor", server.Name, "error", err)
	}
}

// readLoop atiende los mensajes del agent hasta que el canal se corta.
//
// Un agent que no dice nada durante OfflineAfter se da por muerto y se le cierra
// la conexión: es el mismo umbral con el que el panel lo marca caído.
func (s *Server) readLoop(ctx context.Context, conn *websocket.Conn, server store.Server, limits channelLimits) error {
	for {
		readCtx, cancel := context.WithTimeout(ctx, s.cfg.OfflineAfter)
		messageType, data, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			return err
		}

		if messageType != websocket.MessageText {
			s.replyError(ctx, conn, uuid.Nil, proto.CodeInvalidMessage, "los mensajes son JSON de texto")
			continue
		}

		if err := s.handleAgentMessage(ctx, conn, server, limits, data); err != nil {
			return err
		}
	}
}

// handleAgentMessage procesa un mensaje. Devuelve error solo cuando el canal ya
// no sirve: un mensaje inválido se contesta con un error del protocolo y la
// conexión sigue.
func (s *Server) handleAgentMessage(ctx context.Context, conn *websocket.Conn, server store.Server, limits channelLimits, raw []byte) error {
	envelope, err := proto.ParseEnvelope(raw)
	if err != nil {
		code := proto.CodeInvalidMessage
		if errors.Is(err, proto.ErrUnsupportedVersion) {
			code = proto.CodeUnsupportedVersion
		}
		s.log.WarnContext(ctx, "mensaje rechazado", "servidor", server.Name, "error", err)
		s.replyError(ctx, conn, uuid.Nil, code, err.Error())
		return nil
	}

	switch envelope.Type {
	case proto.TypeHeartbeat:
		return s.handleHeartbeat(ctx, conn, server, envelope)

	case proto.TypeMetrics:
		return s.handleMetrics(ctx, conn, server, limits.metrics, envelope)

	case proto.TypeCommandAck:
		s.handleCommandAck(ctx, server, envelope)
		return nil

	case proto.TypeRuntimeState:
		return s.handleRuntimeState(ctx, conn, server, limits.runtime, envelope)

	default:
		s.replyError(ctx, conn, envelope.ID, proto.CodeInvalidMessage,
			fmt.Sprintf("el agent no puede enviar mensajes de tipo %q", envelope.Type))
		return nil
	}
}

// handleHeartbeat registra que el agent sigue vivo y le contesta.
func (s *Server) handleHeartbeat(ctx context.Context, conn *websocket.Conn, server store.Server, envelope proto.Envelope) error {
	heartbeat, err := proto.Decode[proto.Heartbeat](envelope)
	if err != nil {
		s.replyError(ctx, conn, envelope.ID, proto.CodeInvalidMessage, err.Error())
		return nil
	}

	if _, err := s.queries.TouchServer(ctx, store.TouchServerParams{
		ID:           server.ID,
		AgentVersion: &heartbeat.AgentVersion,
	}); err != nil {
		s.log.ErrorContext(ctx, "no se pudo registrar el latido",
			"servidor", server.Name, "error", err)
		s.replyError(ctx, conn, envelope.ID, proto.CodeInternal, "no se pudo registrar el latido")
		return nil
	}

	s.log.DebugContext(ctx, "latido",
		"servidor", server.Name,
		"version_agent", heartbeat.AgentVersion,
		"memoria_agent_bytes", heartbeat.SelfMemoryBytes,
		"cpu_agent_porcentaje", heartbeat.SelfCPUPercent,
	)

	return s.writeMessage(ctx, conn, proto.HeartbeatAck{ServerTime: time.Now().UTC()})
}

// writeMessage manda un mensaje del protocolo por el canal.
func (s *Server) writeMessage(ctx context.Context, conn *websocket.Conn, message proto.Message) error {
	raw, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("serializar %s: %w", message.Type(), err)
	}

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	if err := conn.Write(writeCtx, websocket.MessageText, raw); err != nil {
		return fmt.Errorf("enviar %s: %w", message.Type(), err)
	}
	return nil
}

// replyError contesta un error del protocolo sin cortar la conexión. Si no se
// puede ni escribir el error, se registra y se sigue: el canal se va a caer solo.
func (s *Server) replyError(ctx context.Context, conn *websocket.Conn, inReplyTo uuid.UUID, code proto.ErrorCode, message string) {
	if err := s.writeMessage(ctx, conn, proto.NewError(code, message, inReplyTo)); err != nil {
		s.log.WarnContext(ctx, "no se pudo enviar el error al agent", "error", err)
	}
}

// bearerCredential saca la credencial del encabezado Authorization.
func bearerCredential(r *http.Request) (auth.Secret, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return auth.Secret{}, false
	}

	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return auth.Secret{}, false
	}

	value := strings.TrimSpace(header[len(prefix):])
	if value == "" {
		return auth.Secret{}, false
	}
	return auth.ParseSecret(value), true
}
