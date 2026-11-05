package unithelper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"syscall"

	"github.com/google/uuid"
)

// ErrUnavailable es que el ayudante no está en este servidor: lo instala
// install-agent.sh --enable-deploy.
var ErrUnavailable = errors.New("el ayudante de deploy no está instalado en este servidor")

// Error es un pedido que el ayudante rechazó o que terminó mal.
type Error struct {
	Code     Code
	Message  string
	Unit     string
	ExitCode int
	// Missing son las variables requeridas que faltan, si el chequeo las miró.
	Missing []string
}

func (e *Error) Error() string {
	return e.Message
}

// Client le habla al ayudante por su socket. Lo usa el agent.
type Client struct {
	// Socket es la ruta del socket del ayudante.
	Socket string
}

// NewClient arma un cliente; con socket vacío usa DefaultSocket.
func NewClient(socket string) *Client {
	if socket == "" {
		socket = DefaultSocket
	}
	return &Client{Socket: socket}
}

// Check pregunta si el proyecto se puede desplegar. No cambia nada.
func (c *Client) Check(ctx context.Context, project string) error {
	return c.do(ctx, Request{Op: OpCheck, Project: project})
}

// RunStep corre un paso declarado y espera a que termine. Si ctx se cancela,
// la conexión se cierra y el ayudante para el paso.
func (c *Client) RunStep(ctx context.Context, project string, phase Phase, index int, deploy uuid.UUID) error {
	return c.do(ctx, Request{Op: OpRunStep, Project: project, Phase: phase, Index: index, Deploy: deploy.String()})
}

// StopStep para un paso que quedó corriendo de un deploy anterior.
func (c *Client) StopStep(ctx context.Context, project string, phase Phase, index int, deploy uuid.UUID) error {
	return c.do(ctx, Request{Op: OpStopStep, Project: project, Phase: phase, Index: index, Deploy: deploy.String()})
}

// RestartUnits reinicia las unidades declaradas del proyecto.
func (c *Client) RestartUnits(ctx context.Context, project string) error {
	return c.do(ctx, Request{Op: OpRestartUnits, Project: project})
}

// RestartUnit reinicia una sola de las unidades declaradas del proyecto.
func (c *Client) RestartUnit(ctx context.Context, project, unit string) error {
	return c.do(ctx, Request{Op: OpRestartUnits, Project: project, Unit: unit})
}

// StopUnits para las unidades declaradas del proyecto.
func (c *Client) StopUnits(ctx context.Context, project string) error {
	return c.do(ctx, Request{Op: OpStopUnits, Project: project})
}

// CheckEnv pregunta qué variables requeridas faltan en el archivo de entorno
// del proyecto (ADR-022). checked es falso si el ayudante no lo pudo mirar o
// el proyecto no declara ninguna. Solo viajan nombres.
func (c *Client) CheckEnv(ctx context.Context, project string) (missing []string, checked bool, err error) {
	resp, err := c.exchange(ctx, Request{Op: OpCheck, Project: project})
	if err != nil {
		return nil, false, err
	}
	return resp.Missing, resp.EnvChecked, nil
}

// EnableUnits habilita las unidades declaradas del proyecto para el arranque.
func (c *Client) EnableUnits(ctx context.Context, project string) error {
	return c.do(ctx, Request{Op: OpEnableUnits, Project: project})
}

// do manda un pedido y espera la respuesta. Una conexión por pedido.
func (c *Client) do(ctx context.Context, req Request) error {
	resp, err := c.exchange(ctx, req)
	if err != nil {
		return err
	}
	if resp.OK {
		return nil
	}
	if resp.Code == "" {
		resp.Code = CodeInternal
	}
	return &Error{Code: resp.Code, Message: resp.Message, Unit: resp.Unit, ExitCode: resp.ExitCode, Missing: resp.Missing}
}

// exchange manda un pedido y devuelve la respuesta tal cual, buena o mala.
func (c *Client) exchange(ctx context.Context, req Request) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, &Error{Code: CodeInvalidRequest, Message: err.Error()}
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return Response{}, ErrUnavailable
		}
		return Response{}, fmt.Errorf("conectar con el ayudante: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Cerrar la conexión es la forma de cortar: el ayudante lo toma como que
	// el agent se fue y para lo que estaba corriendo.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	raw, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return Response{}, c.failed(ctx, "mandar el pedido al ayudante", err)
	}

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return Response{}, c.failed(ctx, "leer la respuesta del ayudante", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, fmt.Errorf("la respuesta del ayudante no se entiende: %w", err)
	}
	return resp, nil
}

// failed arma el error de una conexión que se cortó: si fue porque se
// canceló el pedido, eso es lo que hay que informar.
func (c *Client) failed(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("%s: %w", what, err)
}
