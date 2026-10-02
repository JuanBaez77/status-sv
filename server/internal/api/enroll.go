package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/audit"
	"github.com/JuanBaez77/status-sv/server/internal/auth"
	"github.com/JuanBaez77/status-sv/server/internal/enrollment"
)

// handleEnroll canjea un token de un solo uso por la credencial propia del agent.
//
// Es el único endpoint que acepta un secreto sin autenticación previa, así que
// tiene límite de frecuencia por IP y responde lo mismo para token inexistente,
// usado o vencido: distinguirlos le daría pistas a quien esté probando.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := s.clientIP(r)

	if !s.enrollLimiter.allow(ip) {
		s.log.WarnContext(ctx, "enrolamiento rechazado por límite de frecuencia", "ip", ip)
		s.recordBlocked(ctx, audit.Entry{
			Actor: audit.Unknown, ActorType: audit.ActorAgent, Action: audit.ActionAgentEnroll,
			TargetType: "server", TargetID: audit.Unknown, IP: ip,
		})
		s.writeProtoError(ctx, w, http.StatusTooManyRequests, uuid.Nil,
			proto.CodeRateLimited, "demasiados intentos de enrolamiento; esperá un minuto")
		return
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeProtoError(ctx, w, http.StatusBadRequest, uuid.Nil,
			proto.CodeInvalidMessage, "no se pudo leer el pedido")
		return
	}

	envelope, err := proto.ParseEnvelope(raw)
	if err != nil {
		code := proto.CodeInvalidMessage
		if errors.Is(err, proto.ErrUnsupportedVersion) {
			code = proto.CodeUnsupportedVersion
		}
		s.log.WarnContext(ctx, "envelope de enrolamiento rechazado", "ip", ip, "error", err)
		s.writeProtoError(ctx, w, http.StatusBadRequest, uuid.Nil, code, err.Error())
		return
	}

	message, err := proto.Decode[proto.Enroll](envelope)
	if err != nil {
		s.log.WarnContext(ctx, "mensaje de enrolamiento inválido", "ip", ip, "error", err)
		s.writeProtoError(ctx, w, http.StatusBadRequest, envelope.ID,
			proto.CodeInvalidMessage, err.Error())
		return
	}

	result, err := s.enrollment.Enroll(ctx, auth.ParseSecret(message.Token), message.Hostname, message.AgentVersion)
	if err != nil {
		s.writeEnrollFailure(ctx, w, envelope.ID, ip, err)
		return
	}

	s.record(ctx, audit.Entry{
		Actor: result.Server.Name, ActorType: audit.ActorAgent, Action: audit.ActionAgentEnroll,
		TargetType: "server", TargetID: result.Server.ID.String(), Result: audit.ResultOK, IP: ip,
		Detail: map[string]any{"servidor": result.Server.Name},
	})

	ack := proto.EnrollAck{
		ServerID:   result.Server.ID,
		ServerName: result.Server.Name,
		Credential: result.Credential.Reveal(),
		Config:     proto.DefaultAgentConfig(),
	}

	response, err := proto.Marshal(ack)
	if err != nil {
		// Acá el enrolamiento ya está confirmado en la base, pero el agent no se
		// entera de su credencial: queda registrado para poder revocarla.
		s.log.ErrorContext(ctx, "no se pudo armar la respuesta de enrolamiento",
			"servidor_id", result.Server.ID, "error", err)
		s.writeProtoError(ctx, w, http.StatusInternalServerError, envelope.ID,
			proto.CodeInternal, "no se pudo armar la respuesta")
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(response); err != nil {
		s.log.ErrorContext(ctx, "no se pudo enviar la credencial al agent",
			"servidor_id", result.Server.ID, "error", err)
	}
}

// writeEnrollFailure traduce el error del servicio a una respuesta, sin contarle
// al cliente más de lo necesario.
func (s *Server) writeEnrollFailure(ctx context.Context, w http.ResponseWriter, inReplyTo uuid.UUID, ip string, err error) {
	switch {
	case errors.Is(err, enrollment.ErrTokenNotFound), errors.Is(err, enrollment.ErrWrongSecretKind):
		s.log.WarnContext(ctx, "enrolamiento rechazado", "ip", ip, "motivo", err.Error())
		// Del pedido no se guarda nada: ni el token ni el nombre de máquina, que
		// los escribió quien no pudo enrolarse.
		s.record(ctx, audit.Entry{
			Actor: audit.Unknown, ActorType: audit.ActorAgent, Action: audit.ActionAgentEnroll,
			TargetType: "server", TargetID: audit.Unknown, Result: audit.ResultRejected, IP: ip,
			Detail: map[string]any{"motivo": "token inválido, usado o vencido"},
		})
		s.writeProtoError(ctx, w, http.StatusUnauthorized, inReplyTo,
			proto.CodeInvalidToken, "el token no existe, ya se usó o venció")

	case errors.Is(err, enrollment.ErrHostnameRequired):
		s.writeProtoError(ctx, w, http.StatusBadRequest, inReplyTo,
			proto.CodeInvalidMessage, err.Error())

	default:
		s.log.ErrorContext(ctx, "falló el enrolamiento", "ip", ip, "error", err)
		s.writeProtoError(ctx, w, http.StatusInternalServerError, inReplyTo,
			proto.CodeInternal, "no se pudo completar el enrolamiento")
	}
}

// writeProtoError responde con un mensaje de error del protocolo.
func (s *Server) writeProtoError(
	ctx context.Context,
	w http.ResponseWriter,
	status int,
	inReplyTo uuid.UUID,
	code proto.ErrorCode,
	message string,
) {
	envelope, err := proto.Encode(proto.NewError(code, message, inReplyTo))
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudo armar el error del protocolo", "error", err)
		http.Error(w, `{"code":"internal"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(envelope); err != nil {
		s.log.ErrorContext(ctx, "no se pudo escribir el error", "error", err)
	}
}
