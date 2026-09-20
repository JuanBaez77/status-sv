// Package ws es el cliente del control plane: enrola el agent y después
// mantiene el canal permanente por el que viajan el latido y, más adelante, las
// métricas y los comandos.
package ws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/state"
	"github.com/JuanBaez77/status-sv/proto"
)

const (
	// enrollTimeout es lo máximo que puede tardar el enrolamiento entero.
	enrollTimeout = 30 * time.Second
	// maxResponseBytes acota la respuesta del control plane.
	maxResponseBytes = 1 << 20
)

// ErrEnrollRejected es que el control plane rechazó el enrolamiento. Lleva el
// código del protocolo para que quien llama sepa si reintentar sirve de algo.
type ErrEnrollRejected struct {
	Code    proto.ErrorCode
	Message string
}

func (e *ErrEnrollRejected) Error() string {
	return fmt.Sprintf("el control plane rechazó el enrolamiento (%s): %s", e.Code, e.Message)
}

// Retryable dice si tiene sentido volver a intentar con el mismo token.
func (e *ErrEnrollRejected) Retryable() bool {
	return proto.ErrorMessage{Code: e.Code}.Retryable()
}

// Enroll canjea el token de un solo uso por la credencial propia del agent y
// devuelve el estado que hay que guardar en disco.
func Enroll(ctx context.Context, serverURL, token, hostname, agentVersion, osName, arch string) (state.State, error) {
	base, err := normalizeServerURL(serverURL)
	if err != nil {
		return state.State{}, err
	}

	message := proto.Enroll{
		Token:        token,
		Hostname:     hostname,
		AgentVersion: agentVersion,
		OS:           osName,
		Arch:         arch,
	}
	body, err := proto.Marshal(message)
	if err != nil {
		return state.State{}, fmt.Errorf("armar el pedido de enrolamiento: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, enrollTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/agents/enroll", bytes.NewReader(body))
	if err != nil {
		return state.State{}, fmt.Errorf("armar el pedido HTTP: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "status-sv-agent/"+agentVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return state.State{}, fmt.Errorf("contactar al control plane en %s: %w", base, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return state.State{}, fmt.Errorf("leer la respuesta: %w", err)
	}

	envelope, err := proto.ParseEnvelope(raw)
	if err != nil {
		return state.State{}, fmt.Errorf("la respuesta del control plane no se entiende (estado %d): %w", resp.StatusCode, err)
	}

	if envelope.Type == proto.TypeError {
		rejection, decodeErr := proto.Decode[proto.ErrorMessage](envelope)
		if decodeErr != nil {
			return state.State{}, fmt.Errorf("el control plane rechazó el enrolamiento con estado %d", resp.StatusCode)
		}
		return state.State{}, &ErrEnrollRejected{Code: rejection.Code, Message: rejection.Message}
	}

	ack, err := proto.Decode[proto.EnrollAck](envelope)
	if err != nil {
		return state.State{}, fmt.Errorf("leer la respuesta de enrolamiento: %w", err)
	}

	return state.State{
		ServerURL:  base,
		ServerID:   ack.ServerID,
		ServerName: ack.ServerName,
		Credential: ack.Credential,
		EnrolledAt: time.Now().UTC(),
	}, nil
}

// normalizeServerURL valida la dirección del control plane y le saca la barra
// final, para poder concatenar rutas sin terminar con dos barras.
func normalizeServerURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("falta la dirección del control plane")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("la dirección del control plane no es válida (%q): %w", raw, err)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("la dirección del control plane no tiene host: %q", raw)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("la dirección del control plane tiene que ser http o https, no %q", parsed.Scheme)
	}

	return strings.TrimRight(parsed.Scheme+"://"+parsed.Host+parsed.Path, "/"), nil
}

// websocketURL convierte la dirección del control plane en la del canal.
func websocketURL(serverURL string) (string, error) {
	base, err := normalizeServerURL(serverURL)
	if err != nil {
		return "", err
	}
	// https pasa a wss y http a ws; el canal usa el mismo transporte que la API.
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	return base + "/v1/agents/connect", nil
}
