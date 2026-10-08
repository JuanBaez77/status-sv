package unithelper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/JuanBaez77/status-sv/internal/projectspec"
	"github.com/JuanBaez77/status-sv/proto"
)

// Tiempos del ayudante.
const (
	// requestTimeout es lo que se espera a que llegue el pedido.
	requestTimeout = 10 * time.Second
	// stepMargin es lo que se le suma al tiempo del paso antes de dar el
	// pedido por colgado: el que corta el paso es systemd, con el tiempo del
	// propio paso; esto es por si systemd no contesta.
	stepMargin = 30 * time.Second
	// restartTimeout acota el reinicio, o la parada, de cada unidad.
	restartTimeout = 2 * time.Minute
	// cleanupTimeout acota parar un paso cuando el agent se fue.
	cleanupTimeout = 30 * time.Second
)

// Step es un paso listo para correr: lo que el ayudante le pide a systemd.
type Step struct {
	// Unit es el nombre de la unidad transitoria.
	Unit        string
	Description string
	// User es el usuario con el que corre.
	User string
	// Workdir es donde corre.
	Workdir string
	// Argv es el programa, como ruta absoluta, y sus argumentos.
	Argv []string
	// EnvFile es el archivo de entorno, o vacío. Lo lee systemd.
	EnvFile string
	Timeout time.Duration
}

// Outcome es cómo terminó un paso.
type Outcome struct {
	// OK es que terminó con código 0.
	OK bool
	// Result es el motivo según systemd: exit-code, timeout, signal…
	Result   string
	ExitCode int
}

// Manager es lo que el ayudante le pide a systemd. Lo cumple el cliente de
// D-Bus; los tests lo reemplazan.
type Manager interface {
	// RunStep arranca la unidad transitoria del paso y espera a que termine.
	RunStep(ctx context.Context, step Step) (Outcome, error)
	// Stop para una unidad. Que no exista no es un error.
	Stop(ctx context.Context, unit string) error
	// Restart reinicia una unidad y espera a que arranque.
	Restart(ctx context.Context, unit string) error
	// Loaded dice si systemd conoce la unidad.
	Loaded(ctx context.Context, unit string) (bool, error)
}

// Helper atiende los pedidos del agent.
type Helper struct {
	// ProjectsDir es donde se declaran los proyectos.
	ProjectsDir string
	// Load lee la declaración de un proyecto.
	Load func(dir, name string) (projectspec.Spec, error)
	// Systemd es por donde se le habla a systemd.
	Systemd Manager
	// UserExists dice si existe un usuario del sistema.
	UserExists func(name string) bool
	Log        *slog.Logger
}

// Serve atiende un pedido: lo lee de conn, lo ejecuta y contesta. Si conn se
// cierra mientras corre un paso, lo para: un paso no queda corriendo sin un
// deploy que lo espere.
func (h *Helper) Serve(ctx context.Context, conn io.ReadWriter) {
	type read struct {
		req Request
		err error
	}
	got := make(chan read, 1)
	go func() {
		req, err := readRequest(conn)
		got <- read{req, err}
	}()

	var req Request
	select {
	case r := <-got:
		if r.err != nil {
			h.reply(conn, Request{}, Response{Code: CodeInvalidRequest, Message: r.err.Error()})
			return
		}
		req = r.req
	case <-time.After(requestTimeout):
		h.reply(conn, Request{}, Response{Code: CodeInvalidRequest, Message: "no llegó un pedido a tiempo"})
		return
	case <-ctx.Done():
		return
	}

	// Después del pedido el agent no manda nada más: si la conexión deja de
	// leerse, es que se fue.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, conn)
		cancel()
	}()

	resp := h.handle(ctx, req)
	if ctx.Err() != nil {
		// Nadie espera la respuesta.
		h.Log.Warn("el agent se fue antes de que terminara el pedido", "op", req.Op, "proyecto", req.Project)
		return
	}
	h.reply(conn, req, resp)
}

func (h *Helper) reply(w io.Writer, req Request, resp Response) {
	if !resp.OK {
		h.Log.Warn("pedido rechazado o fallido", "op", req.Op, "proyecto", req.Project, "codigo", resp.Code, "detalle", resp.Message)
	} else {
		h.Log.Info("pedido atendido", "op", req.Op, "proyecto", req.Project, "fase", req.Phase, "paso", req.Index)
	}
	if err := writeResponse(w, resp); err != nil {
		h.Log.Warn("no se pudo contestar", "error", err)
	}
}

func (h *Helper) handle(ctx context.Context, req Request) Response {
	spec, err := h.Load(h.ProjectsDir, req.Project)
	if err != nil {
		// El motivo no viaja: puede nombrar un campo del archivo, y ya lo
		// informa el agent por su lado al leer los proyectos.
		return Response{Code: CodeNotDeclared, Message: "el proyecto no está declarado en este servidor o su archivo es inválido"}
	}
	if spec.Project.Runtime != proto.RuntimeSystemd {
		return Response{Code: CodeNotDeclared, Message: "el proyecto no es de systemd"}
	}

	switch req.Op {
	case OpCheck:
		return h.check(ctx, spec)
	case OpRunStep:
		return h.runStep(ctx, req, spec)
	case OpStopStep:
		if _, ok := stepOf(spec, req.Phase, req.Index); !ok {
			return Response{Code: CodeNoSuchStep, Message: "el proyecto no declara ese paso"}
		}
		if err := h.Systemd.Stop(ctx, StepUnit(req.Deploy, req.Phase, req.Index)); err != nil {
			return internal("parar el paso", err)
		}
		return Response{OK: true}
	case OpRestartUnits:
		return h.restart(ctx, spec)
	case OpStopUnits:
		return h.stop(ctx, spec)
	default:
		return Response{Code: CodeInvalidRequest, Message: "operación desconocida"}
	}
}

// check revisa lo que solo root puede ver, sin cambiar nada.
func (h *Helper) check(ctx context.Context, spec projectspec.Spec) Response {
	if spec.RunAs != "" && !h.UserExists(spec.RunAs) {
		return Response{Code: CodeNotReady, Message: "el usuario de run_as no existe en este servidor"}
	}
	if spec.EnvFile != "" {
		if info, err := os.Stat(spec.EnvFile); err != nil || !info.Mode().IsRegular() {
			return Response{Code: CodeNotReady, Message: "el archivo de env_file no existe o no es un archivo"}
		}
	}
	for _, unit := range spec.Project.Units {
		loaded, err := h.Systemd.Loaded(ctx, unit)
		if err != nil {
			return internal("consultar "+unit, err)
		}
		if !loaded {
			return Response{Code: CodeNotReady, Unit: unit, Message: "systemd no conoce la unidad " + unit}
		}
	}
	return Response{OK: true}
}

func (h *Helper) runStep(ctx context.Context, req Request, spec projectspec.Spec) Response {
	declared, ok := stepOf(spec, req.Phase, req.Index)
	if !ok {
		return Response{Code: CodeNoSuchStep, Message: "el proyecto no declara ese paso"}
	}
	if spec.RunAs == "" || declared.Exec == "" {
		// El parser no deja pasar un paso sin usuario ni sin programa; si
		// llegara, no se corre como root.
		return Response{Code: CodeNotReady, Message: "el paso no tiene usuario o programa"}
	}

	step := Step{
		Unit:        StepUnit(req.Deploy, req.Phase, req.Index),
		Description: fmt.Sprintf("status-sv: paso %s %d de %s", req.Phase, req.Index, spec.Project.Name),
		User:        spec.RunAs,
		Workdir:     spec.Project.Workdir,
		Argv:        append([]string{declared.Exec}, declared.Command[1:]...),
		EnvFile:     spec.EnvFile,
		Timeout:     declared.Timeout,
	}

	runCtx, cancel := context.WithTimeout(ctx, declared.Timeout+stepMargin)
	defer cancel()
	outcome, err := h.Systemd.RunStep(runCtx, step)
	if ctx.Err() != nil {
		// El agent se fue: el paso se para aunque el contexto ya esté cancelado.
		stopCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer stop()
		if err := h.Systemd.Stop(stopCtx, step.Unit); err != nil {
			h.Log.Error("no se pudo parar un paso sin dueño", "unidad", step.Unit, "error", err)
		}
		return Response{Code: CodeInternal, Message: "el pedido se cortó"}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return Response{Code: CodeTimeout, Message: "el paso se pasó de su tiempo"}
	case err != nil:
		return internal("correr el paso", err)
	case outcome.OK:
		return Response{OK: true}
	case outcome.Result == "timeout":
		return Response{Code: CodeTimeout, Message: "el paso se pasó de su tiempo"}
	default:
		return Response{Code: CodeStepFailed, ExitCode: outcome.ExitCode, Message: "el paso terminó mal: " + outcome.Result}
	}
}

func (h *Helper) restart(ctx context.Context, spec projectspec.Spec) Response {
	for _, unit := range spec.Project.Units {
		unitCtx, cancel := context.WithTimeout(ctx, restartTimeout)
		err := h.Systemd.Restart(unitCtx, unit)
		cancel()
		if err != nil {
			h.Log.Error("no se pudo reiniciar una unidad", "unidad", unit, "error", err)
			return Response{Code: CodeRestartFailed, Unit: unit, Message: "no se pudo reiniciar " + unit}
		}
	}
	return Response{OK: true}
}

// stop para las unidades declaradas, en el orden inverso al de arranque. No
// se queda en la primera que falla: lo que se pueda parar, se para.
func (h *Helper) stop(ctx context.Context, spec projectspec.Spec) Response {
	failed := ""
	for _, unit := range slices.Backward(spec.Project.Units) {
		unitCtx, cancel := context.WithTimeout(ctx, restartTimeout)
		err := h.Systemd.Stop(unitCtx, unit)
		cancel()
		if err != nil {
			h.Log.Error("no se pudo parar una unidad", "unidad", unit, "error", err)
			failed = unit
		}
	}
	if failed != "" {
		return Response{Code: CodeStopFailed, Unit: failed, Message: "no se pudo parar " + failed}
	}
	return Response{OK: true}
}

// stepOf busca un paso declarado por su fase y su número.
func stepOf(spec projectspec.Spec, phase Phase, index int) (projectspec.Step, bool) {
	var steps []projectspec.Step
	switch phase {
	case PhaseBuild:
		steps = spec.Build
	case PhasePre:
		steps = spec.Pre
	case PhasePost:
		steps = spec.Post
	}
	if index < 0 || index >= len(steps) {
		return projectspec.Step{}, false
	}
	return steps[index], true
}

// internal arma la respuesta de un error que no es del pedido. El detalle
// queda en el registro del ayudante; al agent le llega qué se intentaba.
func internal(what string, err error) Response {
	return Response{Code: CodeInternal, Message: fmt.Sprintf("no se pudo %s: %v", what, err)}
}
