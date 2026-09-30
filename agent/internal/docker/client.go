// Package docker lee los contenedores de Docker hablando directo con su socket.
//
// Es un cliente propio y mínimo, no el SDK oficial, que suma entre 8 y 10 MB
// al binario del agent para tres endpoints que devuelven JSON (ADR-011). Para
// observar hace pedidos de lectura: listar contenedores, inspeccionar uno y
// escuchar el stream de eventos. El motor de deploy suma etiquetar imágenes
// para poder volver atrás (ADR-015), en images.go.
package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DefaultSocket es donde Docker escucha en Linux.
const DefaultSocket = "/run/docker.sock"

const (
	// requestTimeout acota cada pedido que no es el stream de eventos.
	requestTimeout = 10 * time.Second
	// maxResponseBytes acota lo que se lee de una respuesta. Una lista de 200
	// contenedores con todas sus etiquetas ocupa unos cientos de kilobytes.
	maxResponseBytes = 8 << 20
)

// Errores que distinguen por qué no se pudo hablar con Docker. El adaptador
// los traduce al estado de la fuente: sin socket es "no hay Docker", sin
// permiso es un problema de instalación.
var (
	ErrNoSocket         = errors.New("no existe el socket de Docker")
	ErrPermissionDenied = errors.New("sin permiso para el socket de Docker")
)

// Client habla con el demonio de Docker por HTTP.
//
// Los pedidos van sin versión en la ruta (/containers/json y no
// /v1.47/containers/json): Docker los atiende con su versión actual. Los
// campos que se usan existen sin cambios desde hace años, y así el agent no
// deja de andar cuando Docker sube su versión mínima de API.
type Client struct {
	http *http.Client
	base string
}

// NewClient arma un cliente que habla por el socket unix socket.
func NewClient(socket string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			conn, err := d.DialContext(ctx, "unix", socket)
			switch {
			case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOENT):
				return nil, fmt.Errorf("%w: %s", ErrNoSocket, socket)
			case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES):
				return nil, fmt.Errorf("%w: %s", ErrPermissionDenied, socket)
			}
			return conn, err
		},
		MaxIdleConns:    2,
		IdleConnTimeout: 90 * time.Second,
	}
	// El host no importa: la conexión siempre va al socket.
	return newClient(&http.Client{Transport: transport}, "http://docker")
}

func newClient(hc *http.Client, base string) *Client {
	return &Client{http: hc, base: base}
}

// Container es lo que devuelve /containers/json, con los campos que se usan.
type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
}

// List devuelve todos los contenedores, corriendo o no.
func (c *Client) List(ctx context.Context) ([]Container, error) {
	var containers []Container
	if err := c.getJSON(ctx, "/containers/json", url.Values{"all": {"1"}}, &containers); err != nil {
		return nil, fmt.Errorf("listar contenedores: %w", err)
	}
	return containers, nil
}

// Inspection es lo que se toma de /containers/{id}/json: el momento en que
// arrancó y el estado del healthcheck, que la lista no trae.
type Inspection struct {
	State struct {
		Status    string    `json:"Status"`
		ExitCode  int       `json:"ExitCode"`
		StartedAt time.Time `json:"StartedAt"`
		Health    *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	// Config.Tty dice si el contenedor tiene terminal, que cambia el formato
	// de sus logs.
	Config struct {
		Tty bool `json:"Tty"`
	} `json:"Config"`
}

// Inspect devuelve los detalles de un contenedor.
func (c *Client) Inspect(ctx context.Context, id string) (Inspection, error) {
	var inspection Inspection
	if err := c.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, &inspection); err != nil {
		return Inspection{}, fmt.Errorf("inspeccionar el contenedor %s: %w", shortID(id), err)
	}
	return inspection, nil
}

// Event es un evento del stream de Docker, con lo que hace falta para decidir
// si cambió algo que el panel muestra.
type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID string `json:"ID"`
	} `json:"Actor"`
}

// watchedActions son los eventos de contenedor que cambian lo que se muestra.
// Quedan afuera a propósito los exec_*: Docker corre cada healthcheck como un
// exec, así que con un healthcheck cada 10 s llegarían tres eventos cada 10 s
// por contenedor sin que nada cambie.
var watchedActions = []string{
	"create", "start", "restart", "die", "stop", "kill", "oom",
	"pause", "unpause", "rename", "destroy", "health_status",
}

// Events abre el stream de eventos de contenedores. Llama a onOpen cuando el
// stream quedó abierto y a onEvent con cada evento, hasta que se cancela ctx o
// se corta el stream. Siempre devuelve un error: el stream no termina solo.
func (c *Client) Events(ctx context.Context, onOpen func(), onEvent func(Event)) error {
	filters, err := json.Marshal(map[string][]string{
		"type":  {"container"},
		"event": watchedActions,
	})
	if err != nil {
		return fmt.Errorf("armar los filtros de eventos: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/events?"+url.Values{"filters": {string(filters)}}.Encode(), nil)
	if err != nil {
		return fmt.Errorf("armar el pedido de eventos: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("abrir el stream de eventos: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp); err != nil {
		return fmt.Errorf("abrir el stream de eventos: %w", err)
	}
	onOpen()

	decoder := json.NewDecoder(resp.Body)
	for {
		var event Event
		if err := decoder.Decode(&event); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("leer el stream de eventos: %w", err)
		}
		onEvent(event)
	}
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("armar el pedido: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp); err != nil {
		return err
	}

	body := io.LimitReader(resp.Body, maxResponseBytes+1)
	data, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("leer la respuesta: %w", err)
	}
	if len(data) > maxResponseBytes {
		return fmt.Errorf("la respuesta supera %d bytes", maxResponseBytes)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("leer la respuesta: %w", err)
	}
	return nil
}

// ErrNotFound indica que Docker contestó 404: el contenedor ya no existe.
var ErrNotFound = errors.New("el demonio de Docker no lo encontró")

// checkStatus convierte una respuesta de error de Docker en un error de Go.
// El cuerpo trae {"message": "..."}; se toma solo un poco, para el log.
func checkStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body)
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrNotFound, body.Message)
	}
	return fmt.Errorf("el demonio de Docker contestó %d: %s", resp.StatusCode, body.Message)
}

// shortID es el ID recortado como lo muestra `docker ps`, para los logs.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// LogLine es un renglón de log de un contenedor, con su marca de tiempo y el
// stream del que salió.
type LogLine struct {
	TS     time.Time
	Stream string
	Text   string
}

// maxLogBytes acota lo que se lee de un pedido de logs: mil renglones largos
// entran de sobra, y un contenedor que escribe renglones enormes no puede
// hacer que el agent se coma la memoria.
const maxLogBytes = 4 << 20

// Logs trae los últimos renglones del log de un contenedor, del más viejo al
// más nuevo. tty dice si el contenedor tiene terminal: sin terminal, Docker
// multiplexa stdout y stderr con un encabezado de 8 bytes por bloque; con
// terminal manda el texto tal cual.
func (c *Client) Logs(ctx context.Context, id string, lines int, tty bool) ([]LogLine, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	query := url.Values{
		"stdout":     {"1"},
		"stderr":     {"1"},
		"timestamps": {"1"},
		"tail":       {strconv.Itoa(lines)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/containers/"+url.PathEscape(id)+"/logs?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("armar el pedido de logs: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pedir los logs de %s: %w", shortID(id), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp); err != nil {
		return nil, fmt.Errorf("pedir los logs de %s: %w", shortID(id), err)
	}

	body := io.LimitReader(resp.Body, maxLogBytes)
	if tty {
		raw, err := io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("leer los logs de %s: %w", shortID(id), err)
		}
		return splitLogLines(raw, "stdout", nil), nil
	}
	return demuxLogs(body)
}

// demuxLogs separa el stream multiplexado de Docker: cada bloque empieza con
// [stream, 0, 0, 0, largo en 4 bytes big-endian] y trae ese largo de texto.
// Un renglón puede quedar partido entre dos bloques, así que se junta por
// stream antes de cortar en renglones.
func demuxLogs(r io.Reader) ([]LogLine, error) {
	var lines []LogLine
	pending := map[string][]byte{}
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, fmt.Errorf("leer los logs: %w", err)
		}
		stream := "stdout"
		if header[0] == 2 {
			stream = "stderr"
		}
		size := binary.BigEndian.Uint32(header[4:])
		chunk := make([]byte, size)
		if _, err := io.ReadFull(r, chunk); err != nil {
			// Se cortó en el medio (el tope de bytes): lo que se leyó entero sirve.
			break
		}
		lines = append(lines, splitLogLines(append(pending[stream], chunk...), stream, func(rest []byte) {
			pending[stream] = rest
		})...)
	}
	for stream, rest := range pending {
		if len(rest) > 0 {
			lines = append(lines, splitLogLines(append(rest, '\n'), stream, nil)...)
		}
	}
	slices.SortStableFunc(lines, func(a, b LogLine) int { return a.TS.Compare(b.TS) })
	return lines, nil
}

// splitLogLines corta en renglones y separa la marca de tiempo que Docker pone
// adelante con timestamps=1. Si rest no es nil, lo que quedó sin fin de
// renglón se le pasa para juntarlo con el bloque siguiente; si es nil, se
// descarta.
func splitLogLines(data []byte, stream string, rest func([]byte)) []LogLine {
	var lines []LogLine
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSuffix(string(data[:i]), "\r")
		data = data[i+1:]
		ts, text, ok := strings.Cut(line, " ")
		t, err := time.Parse(time.RFC3339Nano, ts)
		if !ok || err != nil {
			lines = append(lines, LogLine{Stream: stream, Text: line})
			continue
		}
		lines = append(lines, LogLine{TS: t.UTC(), Stream: stream, Text: text})
	}
	if rest != nil {
		rest(bytes.Clone(data))
	}
	return lines
}
