package ws

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/JuanBaez77/status-sv/agent/internal/selfstat"
	"github.com/JuanBaez77/status-sv/agent/internal/state"
	"github.com/JuanBaez77/status-sv/proto"
)

const (
	// maxMessageBytes acota lo que se acepta del control plane.
	maxMessageBytes = 1 << 20
	// dialTimeout limita lo que puede tardar abrir el canal.
	dialTimeout = 30 * time.Second
	// writeTimeout limita cada envío.
	writeTimeout = 10 * time.Second
)

// Client mantiene el canal con el control plane.
type Client struct {
	state   state.State
	version string
	log     *slog.Logger

	config  proto.AgentConfig
	sampler *selfstat.Sampler
	backoff *Backoff
	started time.Time

	// silence pisa el plazo de silencio calculado. Los tests lo usan para no
	// tener que esperar tres latidos de verdad.
	silence time.Duration

	// httpClient se reemplaza en los tests.
	httpClient *http.Client
}

// NewClient arma el cliente con el estado guardado en disco.
func NewClient(s state.State, version string, log *slog.Logger) *Client {
	return &Client{
		state:      s,
		version:    version,
		log:        log,
		config:     proto.DefaultAgentConfig(),
		sampler:    selfstat.NewSampler(),
		backoff:    NewBackoff(),
		started:    time.Now(),
		httpClient: &http.Client{Timeout: dialTimeout},
	}
}

// Run mantiene el canal abierto hasta que se cancele el contexto.
//
// Nunca se rinde: si el control plane está caído, si la red se corta o si la
// credencial fue revocada, espera con backoff y vuelve a intentar. Que el agent
// se quede callado sería peor que reintentar, porque el panel lo muestra caído
// y nadie sabría que en realidad el proceso se murió.
func (c *Client) Run(ctx context.Context) error {
	for {
		err := c.session(ctx)

		// Si nos están apagando, que el canal se corte es lo esperado y no un error.
		select {
		case <-ctx.Done():
			c.log.InfoContext(ctx, "agent detenido")
			return nil
		default:
		}

		c.logDisconnection(ctx, err)

		delay := c.backoff.Next()
		c.log.InfoContext(ctx, "reintentando la conexión",
			"espera", delay.Round(time.Millisecond),
			"intento", c.backoff.Attempts(),
		)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// logDisconnection explica la caída con el nivel que corresponde: una credencial
// revocada necesita intervención humana, una red caída no.
func (c *Client) logDisconnection(ctx context.Context, err error) {
	var rejected *ErrUnauthorized
	if errors.As(err, &rejected) {
		c.log.ErrorContext(ctx, "el control plane rechazó la credencial: hay que enrolar el agent de nuevo",
			"servidor", c.state.ServerName)
		return
	}
	c.log.WarnContext(ctx, "canal cerrado", "error", err)
}

// ErrUnauthorized es que el control plane no aceptó la credencial.
type ErrUnauthorized struct{}

func (*ErrUnauthorized) Error() string {
	return "credencial rechazada por el control plane"
}

// session abre el canal y lo atiende hasta que se corta.
func (c *Client) session(ctx context.Context) error {
	target, err := websocketURL(c.state.ServerURL)
	if err != nil {
		return err
	}

	dialCtx, cancelDial := context.WithTimeout(ctx, dialTimeout)
	defer cancelDial()

	conn, resp, err := websocket.Dial(dialCtx, target, &websocket.DialOptions{
		HTTPClient: c.httpClient,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.state.Credential}},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return &ErrUnauthorized{}
		}
		return fmt.Errorf("abrir el canal con %s: %w", target, err)
	}
	conn.SetReadLimit(maxMessageBytes)

	// Este contexto corta las dos goroutines de la sesión en cuanto una termina.
	sessionCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "hasta luego") }()

	// La conexión funcionó: la próxima caída empieza a contar de cero.
	c.backoff.Reset()
	c.log.InfoContext(ctx, "conectado al control plane",
		"servidor", c.state.ServerName,
		"url", c.state.ServerURL,
	)

	errs := make(chan error, 2)
	go func() { errs <- c.heartbeatLoop(sessionCtx, conn) }()
	go func() { errs <- c.readLoop(sessionCtx, conn) }()

	return <-errs
}

// heartbeatLoop manda el latido con el intervalo que indicó el control plane.
func (c *Client) heartbeatLoop(ctx context.Context, conn *websocket.Conn) error {
	// El primero sale enseguida, para que el panel muestre el agent como vivo
	// sin esperar un intervalo entero.
	if err := c.sendHeartbeat(ctx, conn); err != nil {
		return err
	}

	ticker := time.NewTicker(c.config.Heartbeat())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := c.sendHeartbeat(ctx, conn); err != nil {
				return err
			}
		}
	}
}

// sendHeartbeat arma y manda un latido con el consumo del propio agent.
func (c *Client) sendHeartbeat(ctx context.Context, conn *websocket.Conn) error {
	heartbeat := proto.Heartbeat{
		AgentVersion:  c.version,
		UptimeSeconds: int64(time.Since(c.started).Seconds()),
	}

	// Que falle la medición no es motivo para no mandar el latido: el latido
	// dice que el agent está vivo, que es lo importante.
	if sample, err := c.sampler.Sample(); err != nil {
		c.log.WarnContext(ctx, "no se pudo medir el consumo propio", "error", err)
	} else {
		heartbeat.SelfMemoryBytes = sample.MemoryBytes
		heartbeat.SelfCPUPercent = sample.CPUPercent
	}

	raw, err := proto.Marshal(heartbeat)
	if err != nil {
		return fmt.Errorf("armar el latido: %w", err)
	}

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	if err := conn.Write(writeCtx, websocket.MessageText, raw); err != nil {
		return fmt.Errorf("enviar el latido: %w", err)
	}
	return nil
}

// readLoop atiende lo que manda el control plane.
//
// Cada lectura tiene plazo: el control plane contesta un acuse por cada latido,
// así que si pasan varios intervalos sin recibir nada, el canal está muerto
// aunque el sistema operativo todavía no se haya enterado. Pasa cuando la red
// se corta sin cerrar la conexión, que es lo habitual detrás de un NAT.
func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		readCtx, cancel := context.WithTimeout(ctx, c.silenceTimeout())
		_, raw, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			return err
		}

		envelope, err := proto.ParseEnvelope(raw)
		if err != nil {
			c.log.WarnContext(ctx, "mensaje del control plane que no se entiende", "error", err)
			continue
		}

		switch envelope.Type {
		case proto.TypeHeartbeatAck:
			c.handleHeartbeatAck(ctx, envelope)
		case proto.TypeError:
			c.handleServerError(ctx, envelope)
		default:
			c.log.DebugContext(ctx, "mensaje ignorado", "tipo", envelope.Type)
		}
	}
}

// handleHeartbeatAck aplica la configuración nueva, si el control plane mandó una.
func (c *Client) handleHeartbeatAck(ctx context.Context, envelope proto.Envelope) {
	ack, err := proto.Decode[proto.HeartbeatAck](envelope)
	if err != nil {
		c.log.WarnContext(ctx, "acuse de latido inválido", "error", err)
		return
	}

	if ack.Config != nil && *ack.Config != c.config {
		c.log.InfoContext(ctx, "el control plane cambió la configuración",
			"latido_segundos", ack.Config.HeartbeatSeconds,
			"metricas_segundos", ack.Config.MetricsIntervalSeconds,
		)
		c.config = *ack.Config
	}
}

// handleServerError registra un rechazo del control plane.
func (c *Client) handleServerError(ctx context.Context, envelope proto.Envelope) {
	message, err := proto.Decode[proto.ErrorMessage](envelope)
	if err != nil {
		c.log.WarnContext(ctx, "mensaje de error inválido", "error", err)
		return
	}

	c.log.ErrorContext(ctx, "el control plane rechazó un mensaje",
		"codigo", message.Code,
		"detalle", message.Message,
		"reintentable", message.Retryable(),
	)
}

// silenceTimeout es cuánto se tolera no recibir nada del control plane antes de
// dar el canal por muerto. Son tres latidos: uno perdido puede ser una demora,
// tres seguidos no.
func (c *Client) silenceTimeout() time.Duration {
	if c.silence > 0 {
		return c.silence
	}
	heartbeat := c.config.Heartbeat()
	if heartbeat <= 0 {
		heartbeat = proto.DefaultAgentConfig().Heartbeat()
	}
	return 3 * heartbeat
}
