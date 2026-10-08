package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/agent/internal/deploy"
	"github.com/JuanBaez77/status-sv/agent/internal/metrics"
	"github.com/JuanBaez77/status-sv/agent/internal/projects"
	"github.com/JuanBaez77/status-sv/agent/internal/runtimestate"
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

	// config la escribe la lectura del canal cuando el control plane manda una
	// nueva, y la leen el latido, el envío de métricas y el muestreo, cada uno
	// en su goroutine: por eso va con candado.
	configMu sync.Mutex
	config   proto.AgentConfig

	// buffer tiene las muestras que esperan confirmación. Vive más que cada
	// sesión: lo que no se confirmó sale en el canal siguiente.
	buffer *metrics.Buffer
	// metricsRejected evita repetir el mismo aviso con cada lote rechazado. Solo
	// lo usa la lectura del canal, y se reinicia con cada sesión.
	metricsRejected bool
	// metricsWake adelanta el próximo lote cuando llega un acuse y todavía hay
	// muestras esperando: después de un corte, el buffer se vacía sin esperar
	// un intervalo entero por cada lote.
	metricsWake chan struct{}

	// runtime es la bandeja de las fotos de runtime. Es opcional: sin ella el
	// canal no manda fotos (la prueba de carga, por ejemplo).
	runtime *runtimestate.Outbox
	// projects es la bandeja de los proyectos declarados, también opcional.
	projects *projects.Outbox
	// commands ejecuta los comandos del control plane. Es opcional: sin él,
	// todo comando se rechaza.
	commands commandHandler
	// deploys es la bandeja de las fotos de los deploys, también opcional.
	deploys *deploy.Outbox
	// proposals guarda las propuestas de proyectos nuevos (ADR-022), y
	// proposalsChanged avisa que cambiaron. Opcionales: sin ellos, una
	// propuesta se rechaza.
	proposals        proposalHandler
	proposalsChanged func()

	sampler *selfstat.Sampler
	backoff *Backoff
	started time.Time

	// silence pisa el plazo de silencio calculado. Los tests lo usan para no
	// tener que esperar tres latidos de verdad.
	silence time.Duration

	// httpClient se reemplaza en los tests.
	httpClient *http.Client
}

// NewClient arma el cliente con el estado guardado en disco. Las métricas que
// manda las toma de buffer, que llena el muestreo.
func NewClient(s state.State, version string, log *slog.Logger, buffer *metrics.Buffer) *Client {
	return &Client{
		state:       s,
		version:     version,
		log:         log,
		config:      proto.DefaultAgentConfig(),
		buffer:      buffer,
		metricsWake: make(chan struct{}, 1),
		sampler:     selfstat.NewSampler(),
		backoff:     NewBackoff(),
		started:     time.Now(),
		httpClient:  &http.Client{Timeout: dialTimeout},
	}
}

// MetricsInterval devuelve cada cuánto hay que tomar una muestra, según la
// última configuración del control plane.
func (c *Client) MetricsInterval() time.Duration {
	return c.currentConfig().MetricsInterval()
}

func (c *Client) currentConfig() proto.AgentConfig {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	return c.config
}

// setConfig guarda la configuración nueva y dice si cambió algo.
func (c *Client) setConfig(config proto.AgentConfig) bool {
	c.configMu.Lock()
	defer c.configMu.Unlock()

	if config == c.config {
		return false
	}
	c.config = config
	return true
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

	// Este contexto corta las goroutines de la sesión en cuanto una termina.
	sessionCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "hasta luego") }()

	// La conexión funcionó: la próxima caída empieza a contar de cero.
	c.backoff.Reset()
	c.log.InfoContext(ctx, "conectado al control plane",
		"servidor", c.state.ServerName,
		"url", c.state.ServerURL,
		"metricas_pendientes", c.buffer.Len(),
	)

	// La respuesta al lote que estaba en vuelo en el canal anterior no va a
	// llegar: esas muestras salen de nuevo en este.
	c.buffer.Restart()
	c.metricsRejected = false
	if c.runtime != nil {
		c.runtime.Restart()
	}
	if c.projects != nil {
		c.projects.Restart()
	}
	if c.deploys != nil {
		c.deploys.Restart()
	}

	errs := make(chan error, 6)
	go func() { errs <- c.heartbeatLoop(sessionCtx, conn) }()
	go func() { errs <- c.metricsLoop(sessionCtx, conn) }()
	go func() { errs <- c.readLoop(sessionCtx, conn) }()
	if c.runtime != nil {
		go func() { errs <- c.runtimeLoop(sessionCtx, conn) }()
	}
	if c.projects != nil {
		go func() { errs <- c.projectsLoop(sessionCtx, conn) }()
	}
	if c.deploys != nil {
		go func() { errs <- c.deploysLoop(sessionCtx, conn) }()
	}

	return <-errs
}

// heartbeatLoop manda el latido con el intervalo que indicó el control plane.
func (c *Client) heartbeatLoop(ctx context.Context, conn *websocket.Conn) error {
	// El primero sale enseguida, para que el panel muestre el agent como vivo
	// sin esperar un intervalo entero.
	if err := c.sendHeartbeat(ctx, conn); err != nil {
		return err
	}

	for {
		// El intervalo se consulta en cada vuelta: si el control plane lo
		// cambia, vale desde el latido siguiente.
		if err := sleep(ctx, c.currentConfig().Heartbeat()); err != nil {
			return err
		}
		if err := c.sendHeartbeat(ctx, conn); err != nil {
			return err
		}
	}
}

// metricsLoop manda lotes de métricas.
//
// El intervalo de lote se mide desde las muestras y no con un reloj propio:
// sale en cuanto entra la muestra que lo completa. Con la configuración por
// defecto (una muestra y un lote cada 10 s), cada muestra sale apenas se toma;
// con dos relojes independientes podía esperar hasta 10 s en el buffer, y la
// meta es que llegue al panel en menos de 2.
//
// Además sale un lote enseguida al conectar, para vaciar lo que se juntó
// durante un corte, y al llegar un acuse si quedan muestras esperando. Si no
// pasa nada de eso, al cumplirse el intervalo se reintenta lo que haya quedado
// pendiente (un lote sin respuesta o un rechazo reintentable).
func (c *Client) metricsLoop(ctx context.Context, conn *websocket.Conn) error {
	var lastSent time.Time
	for {
		sent, err := c.sendMetrics(ctx, conn)
		if err != nil {
			return err
		}
		if sent {
			lastSent = time.Now()
		}
		if err := c.waitForBatch(ctx, lastSent); err != nil {
			return err
		}
	}
}

// waitForBatch espera el momento de intentar el próximo lote.
func (c *Client) waitForBatch(ctx context.Context, lastSent time.Time) error {
	config := c.currentConfig()
	timer := time.NewTimer(config.MetricsBatch())
	defer timer.Stop()
	due := batchDue(lastSent, config)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-c.metricsWake:
			return nil
		case <-c.buffer.Added():
			if !time.Now().Before(due) {
				return nil
			}
		}
	}
}

// batchDue es desde cuándo una muestra nueva dispara el próximo lote. Lleva
// medio intervalo de muestreo de tolerancia: la muestra que completa el
// intervalo de lote llega con unos milisegundos de diferencia respecto del
// envío anterior, y sin tolerancia se quedaría esperando al lote siguiente.
func batchDue(lastSent time.Time, config proto.AgentConfig) time.Time {
	return lastSent.Add(config.MetricsBatch() - config.MetricsInterval()/2)
}

// sendMetrics manda el próximo lote, si hay uno y no hay otro esperando
// respuesta. Devuelve si mandó algo.
func (c *Client) sendMetrics(ctx context.Context, conn *websocket.Conn) (bool, error) {
	batch := c.buffer.Next(proto.MaxSamplesPerBatch, time.Now())
	if len(batch.Samples) == 0 {
		return false, nil
	}

	raw, id, n, err := encodeMetrics(batch.Samples)
	if err != nil {
		// No pasa con muestras del colector (hay un test que lo asegura), pero
		// si pasara, la muestra trabaría la cola para siempre.
		c.log.ErrorContext(ctx, "se descarta una muestra que no se puede mandar", "error", err)
		c.buffer.DiscardOldest(1)
		return false, nil
	}

	// Se registra antes de escribir: el acuse puede llegar antes de que Write
	// termine de volver.
	c.buffer.Sent(batch, n, id, time.Now())

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, raw); err != nil {
		return false, fmt.Errorf("enviar métricas: %w", err)
	}

	c.log.DebugContext(ctx, "lote de métricas enviado", "muestras", n, "bytes", len(raw))
	return true, nil
}

// encodeMetrics arma el mensaje con todas las muestras que entren en el tope
// del contrato, empezando por las más viejas. Devuelve el mensaje, su ID y
// cuántas muestras lleva.
func encodeMetrics(samples []proto.Sample) ([]byte, uuid.UUID, int, error) {
	n := len(samples)
	for n > 0 {
		envelope, err := proto.Encode(proto.Metrics{Samples: samples[:n]})
		if err != nil {
			return nil, uuid.Nil, 0, fmt.Errorf("armar el lote: %w", err)
		}
		raw, err := json.Marshal(envelope)
		if err != nil {
			return nil, uuid.Nil, 0, fmt.Errorf("serializar el lote: %w", err)
		}
		if len(raw) <= proto.MaxMessageBytes {
			return raw, envelope.ID, n, nil
		}

		// Se achica en proporción a lo que sobró, y al menos en una muestra.
		n = min(n-1, n*proto.MaxMessageBytes/len(raw))
	}
	return nil, uuid.Nil, 0, fmt.Errorf("una sola muestra supera el tope de %d bytes por mensaje", proto.MaxMessageBytes)
}

// sleep espera d o hasta que se cancele el contexto.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
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
		case proto.TypeCommand:
			// El comando corre aparte: la lectura del canal no puede quedar
			// esperando a que termine.
			go c.handleCommand(ctx, conn, envelope)
		case proto.TypeRuntimeStateAck:
			c.handleRuntimeAck(ctx, envelope)
		case proto.TypeProjectsStateAck:
			c.handleProjectsAck(ctx, envelope)
		case proto.TypeDeployEventAck:
			c.handleDeployAck(ctx, envelope)
		case proto.TypeProjectProposal:
			// En orden, no aparte como un comando: guardar y después retirar
			// la misma propuesta no puede llegar al revés. Es escribir dos
			// archivos chicos.
			c.handleProposal(ctx, conn, envelope)
		case proto.TypeMetricsAck:
			c.handleMetricsAck(ctx, envelope)
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

	if ack.Config != nil && c.setConfig(*ack.Config) {
		c.log.InfoContext(ctx, "el control plane cambió la configuración",
			"latido_segundos", ack.Config.HeartbeatSeconds,
			"metricas_segundos", ack.Config.MetricsIntervalSeconds,
			"lote_segundos", ack.Config.MetricsBatchSeconds,
		)
	}
}

// handleMetricsAck suelta del buffer lo que el control plane ya guardó.
func (c *Client) handleMetricsAck(ctx context.Context, envelope proto.Envelope) {
	ack, err := proto.Decode[proto.MetricsAck](envelope)
	if err != nil {
		c.log.WarnContext(ctx, "acuse de métricas inválido", "error", err)
		return
	}

	// Si no se guardó nada, no hay nada que soltar; pero el lote ya tuvo
	// respuesta y puede salir el siguiente.
	lastTS := ack.LastTS
	if ack.Accepted == 0 {
		lastTS = time.Time{}
	}
	released := c.buffer.Ack(lastTS)
	c.metricsRejected = false

	if c.buffer.Len() > 0 {
		select {
		case c.metricsWake <- struct{}{}:
		default: // ya había un aviso pendiente
		}
	}

	c.log.DebugContext(ctx, "métricas confirmadas",
		"aceptadas", ack.Accepted,
		"liberadas", released,
		"pendientes", c.buffer.Len(),
	)
}

// handleServerError registra un rechazo del control plane. Si el rechazo es de
// un lote de métricas, el buffer decide si esas muestras vuelven a salir.
func (c *Client) handleServerError(ctx context.Context, envelope proto.Envelope) {
	message, err := proto.Decode[proto.ErrorMessage](envelope)
	if err != nil {
		c.log.WarnContext(ctx, "mensaje de error inválido", "error", err)
		return
	}

	if message.InReplyTo != nil {
		if c.runtime != nil && c.runtime.Rejected(*message.InReplyTo, message.Retryable()) {
			c.log.WarnContext(ctx, "el control plane rechazó una foto de runtime",
				"codigo", message.Code, "detalle", message.Message, "reintentable", message.Retryable())
			return
		}
		if c.projects != nil && c.projects.Rejected(*message.InReplyTo, message.Retryable()) {
			c.log.WarnContext(ctx, "el control plane rechazó los proyectos declarados",
				"codigo", message.Code, "detalle", message.Message, "reintentable", message.Retryable())
			return
		}
		if c.deploys != nil && c.deploys.Rejected(*message.InReplyTo, message.Retryable()) {
			c.log.WarnContext(ctx, "el control plane rechazó una foto de deploy",
				"codigo", message.Code, "detalle", message.Message, "reintentable", message.Retryable())
			return
		}
		if discarded, matched := c.buffer.Reject(*message.InReplyTo, message.Retryable()); matched {
			c.logMetricsRejection(ctx, message, discarded)
			return
		}
	}

	c.log.ErrorContext(ctx, "el control plane rechazó un mensaje",
		"codigo", message.Code,
		"detalle", message.Message,
		"reintentable", message.Retryable(),
	)
}

// logMetricsRejection avisa que el control plane rechazó un lote. Si rechaza
// uno, suele rechazar todos (por ejemplo, un control plane que todavía no
// guarda métricas), así que el aviso sale una vez por sesión y el resto va a
// depuración.
func (c *Client) logMetricsRejection(ctx context.Context, message proto.ErrorMessage, discarded int) {
	attrs := []any{
		"codigo", message.Code,
		"detalle", message.Message,
		"reintentable", message.Retryable(),
		"descartadas", discarded,
	}
	if c.metricsRejected {
		c.log.DebugContext(ctx, "el control plane rechazó otro lote de métricas", attrs...)
		return
	}
	c.metricsRejected = true
	c.log.WarnContext(ctx, "el control plane rechazó un lote de métricas", attrs...)
}

// silenceTimeout es cuánto se tolera no recibir nada del control plane antes de
// dar el canal por muerto. Son tres latidos: uno perdido puede ser una demora,
// tres seguidos no.
func (c *Client) silenceTimeout() time.Duration {
	if c.silence > 0 {
		return c.silence
	}
	heartbeat := c.currentConfig().Heartbeat()
	if heartbeat <= 0 {
		heartbeat = proto.DefaultAgentConfig().Heartbeat()
	}
	return 3 * heartbeat
}
