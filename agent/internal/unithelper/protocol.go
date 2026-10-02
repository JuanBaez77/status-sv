// Package unithelper es el ayudante root del deploy de proyectos systemd
// (ADR-019), y el cliente con el que el agent le habla.
//
// El agent corre sin privilegios: no puede reiniciar un servicio ni correr
// nada con el usuario de un proyecto. El ayudante sí, y por eso hace muy poco:
// atiende un pedido por conexión, de una lista cerrada, y ninguno trae un
// comando, un usuario ni el nombre de una unidad. Trae el nombre de un proyecto
// declarado y, para un paso, su fase y su número. Qué se corre, con qué usuario
// y qué unidades se reinician lo saca el ayudante del project.yaml que escribió
// root, leyéndolo él mismo en cada pedido.
//
// Un agent comprometido puede pedir que corra un paso declarado o que se
// reinicie un servicio declarado. Nada más.
package unithelper

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

// DefaultSocket es donde escucha el ayudante. Lo crea su unidad de socket,
// como root:statussv 0660.
const DefaultSocket = "/run/status-sv/unit-helper.sock"

// maxRequestBytes acota un pedido. Uno real ocupa menos de 200 bytes.
const maxRequestBytes = 1 << 10

// Op es una operación del ayudante. Es una lista cerrada.
type Op string

// Operaciones del ayudante.
const (
	// OpCheck revisa, sin cambiar nada, que el proyecto se pueda desplegar:
	// que su usuario y su archivo de entorno existan y que sus unidades estén
	// cargadas.
	OpCheck Op = "check"
	// OpRunStep corre un paso declarado y espera a que termine.
	OpRunStep Op = "run_step"
	// OpStopStep para un paso que quedó corriendo.
	OpStopStep Op = "stop_step"
	// OpRestartUnits reinicia las unidades declaradas del proyecto.
	OpRestartUnits Op = "restart_units"
)

// Phase es de qué lista de pasos del project.yaml es un paso.
type Phase string

// Fases de los pasos declarados.
const (
	PhaseBuild Phase = "build"
	PhasePre   Phase = "pre"
	PhasePost  Phase = "post"
)

// Request es un pedido al ayudante. Solo identificadores: no hay ningún campo
// que lleve texto para ejecutar.
type Request struct {
	Op      Op     `json:"op"`
	Project string `json:"project"`
	// Phase, Index y Deploy van solo en run_step y stop_step.
	Phase  Phase  `json:"phase,omitempty"`
	Index  int    `json:"index,omitempty"`
	Deploy string `json:"deploy,omitempty"`
}

// Validate revisa la forma del pedido: la operación, el nombre del proyecto
// y, según la operación, que traiga o no los datos del paso.
func (r Request) Validate() error {
	if !proto.ValidProjectName(r.Project) {
		return errors.New("proyecto inválido")
	}
	switch r.Op {
	case OpCheck, OpRestartUnits:
		if r.Phase != "" || r.Index != 0 || r.Deploy != "" {
			return errors.New("esa operación lleva solo el proyecto")
		}
	case OpRunStep, OpStopStep:
		switch r.Phase {
		case PhaseBuild, PhasePre, PhasePost:
		default:
			return errors.New("fase desconocida")
		}
		if r.Index < 0 {
			return errors.New("número de paso inválido")
		}
		if id, err := uuid.Parse(r.Deploy); err != nil || id == uuid.Nil || id.String() != r.Deploy {
			return errors.New("el deploy tiene que ser un UUID")
		}
	default:
		return errors.New("operación desconocida")
	}
	return nil
}

// Code dice por qué un pedido no terminó bien. Es una lista cerrada: quien lo
// recibe decide por el código, no por el texto.
type Code string

// Códigos de un pedido que no terminó bien.
const (
	// CodeInvalidRequest: el pedido no tiene la forma esperada.
	CodeInvalidRequest Code = "invalid_request"
	// CodeForbidden: quien se conectó no es el agent.
	CodeForbidden Code = "forbidden"
	// CodeNotDeclared: el proyecto no está declarado, su archivo es inválido
	// o no es de systemd.
	CodeNotDeclared Code = "not_declared"
	// CodeNoSuchStep: el proyecto no declara ese paso.
	CodeNoSuchStep Code = "no_such_step"
	// CodeNotReady: falta algo para poder desplegar (el usuario, el archivo de
	// entorno, una unidad).
	CodeNotReady Code = "not_ready"
	// CodeStepFailed: el paso corrió y terminó mal.
	CodeStepFailed Code = "step_failed"
	// CodeTimeout: el paso se pasó de su tiempo.
	CodeTimeout Code = "timeout"
	// CodeRestartFailed: una unidad no se pudo reiniciar.
	CodeRestartFailed Code = "restart_failed"
	// CodeInternal: cualquier otra cosa.
	CodeInternal Code = "internal"
)

// Response es la respuesta del ayudante.
type Response struct {
	OK      bool   `json:"ok"`
	Code    Code   `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	// Unit es la unidad que no se pudo reiniciar.
	Unit string `json:"unit,omitempty"`
	// ExitCode es el código de salida de un paso que terminó mal.
	ExitCode int `json:"exit_code,omitempty"`
}

// StepUnit es el nombre de la unidad transitoria en la que corre un paso. Es
// único por deploy y por paso: el agent lee su salida del diario por ese
// nombre, y un paso de otro deploy no se mezcla.
func StepUnit(deploy string, phase Phase, index int) string {
	return fmt.Sprintf("statussv-step-%s-%s-%d.service", deploy, phase, index)
}

// readRequest lee un pedido: un objeto JSON en un renglón, sin campos de más.
func readRequest(r io.Reader) (Request, error) {
	line, err := bufio.NewReader(io.LimitReader(r, maxRequestBytes+1)).ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return Request{}, errors.New("no llegó un pedido completo")
	}
	if len(line) > maxRequestBytes {
		return Request{}, errors.New("el pedido es demasiado largo")
	}
	decoder := json.NewDecoder(strings.NewReader(line))
	decoder.DisallowUnknownFields()
	var req Request
	if err := decoder.Decode(&req); err != nil {
		return Request{}, errors.New("el pedido no es el JSON esperado")
	}
	if decoder.More() {
		return Request{}, errors.New("el pedido trae más de un objeto")
	}
	if err := req.Validate(); err != nil {
		return Request{}, err
	}
	return req, nil
}

// writeResponse manda la respuesta, en un renglón.
func writeResponse(w io.Writer, resp Response) error {
	raw, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	_, err = w.Write(append(raw, '\n'))
	return err
}

// Reject contesta un pedido que no se va a atender, sin leerlo.
func Reject(w io.Writer, code Code, message string) {
	_ = writeResponse(w, Response{Code: code, Message: message})
}
