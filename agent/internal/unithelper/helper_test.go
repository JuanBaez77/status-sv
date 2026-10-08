package unithelper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/internal/projectspec"
)

const deployID = "0199a1b2-0000-7000-8000-00000000000a"

const facturasYAML = `name: facturas
environment: staging
repo: github.com/extendsystem/facturas
branch: main
runtime: systemd
workdir: /srv/facturas
units:
  - gunicorn-facturas.service
  - facturas-worker.service
run_as: facturas
env_file: ENVFILE
deploy:
  build:
    - command: [".venv/bin/pip", "install", "-r", "requirements.txt"]
      timeout_seconds: 600
    - command: ["./scripts/estaticos.sh"]
  pre:
    - command: [".venv/bin/python", "manage.py", "migrate", "--noinput"]
`

const tiendaYAML = `name: tienda
environment: staging
repo: github.com/extendsystem/tienda
branch: main
runtime: docker
compose_file: compose.yml
workdir: /srv/tienda
`

// fakeSystemd anota lo que se le pide y contesta lo que el test le diga.
type fakeSystemd struct {
	mu       sync.Mutex
	steps    []Step
	stopped  []string
	restarts []string
	outcome  Outcome
	runErr   error
	// gate, si no es nil, frena RunStep hasta que se cierra o se cancela.
	gate chan struct{}
	// failRestart es la unidad que no se puede reiniciar.
	failRestart string
	// failStop es la unidad que no se puede parar.
	failStop string
	// enabled son las unidades habilitadas, y failEnable hace fallar Enable.
	enabled    []string
	failEnable bool
	// missing son las unidades que systemd no conoce.
	missing map[string]bool
}

func (f *fakeSystemd) RunStep(ctx context.Context, step Step) (Outcome, error) {
	f.mu.Lock()
	f.steps = append(f.steps, step)
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		}
	}
	return f.outcome, f.runErr
}

func (f *fakeSystemd) Stop(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, unit)
	if unit != "" && unit == f.failStop {
		return errors.New("el trabajo terminó en failed")
	}
	return nil
}

func (f *fakeSystemd) Restart(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts = append(f.restarts, unit)
	if unit == f.failRestart {
		return errors.New("el trabajo terminó en failed")
	}
	return nil
}

func (f *fakeSystemd) Enable(_ context.Context, units []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failEnable {
		return errors.New("no se pudo")
	}
	f.enabled = append(f.enabled, units...)
	return nil
}

func (f *fakeSystemd) Loaded(_ context.Context, unit string) (bool, error) {
	return !f.missing[unit], nil
}

func (f *fakeSystemd) Stopped() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stopped...)
}

// newHelper arma un ayudante con dos proyectos declarados: facturas (systemd)
// y tienda (Docker).
func newHelper(t *testing.T, sd *fakeSystemd) *Helper {
	t.Helper()
	envFile := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(envFile, []byte("CLAVE=valor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// La ruta del archivo de entorno va con barras, como en un servidor.
	yaml := strings.Replace(facturasYAML, "ENVFILE", "/etc/facturas/env", 1)
	specs := map[string]string{"facturas": yaml, "tienda": tiendaYAML}
	return &Helper{
		ProjectsDir: "/etc/status-sv/projects.d",
		Load: func(_, name string) (projectspec.Spec, error) {
			content, ok := specs[name]
			if !ok {
				return projectspec.Spec{}, errors.New("no declarado")
			}
			spec, err := projectspec.ParseSpec(name+".yaml", []byte(content))
			if err == nil && spec.EnvFile != "" {
				spec.EnvFile = envFile
			}
			return spec, err
		},
		Systemd:    sd,
		UserExists: func(name string) bool { return name == "facturas" },
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// ask manda un pedido crudo al ayudante y devuelve su respuesta.
func ask(t *testing.T, h *Helper, request string) Response {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Serve(t.Context(), server)
		_ = server.Close()
	}()
	// En otra goroutine: la conexión de prueba no tiene búfer, y un pedido que
	// el ayudante deja de leer por largo trabaría al test.
	go func() { _, _ = client.Write([]byte(request + "\n")) }()
	line, err := bufio.NewReader(client).ReadBytes('\n')
	if err != nil {
		t.Fatalf("leer la respuesta: %v", err)
	}
	_ = client.Close()
	<-done
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("la respuesta no es JSON: %s", line)
	}
	return resp
}

func TestRunStepRunsTheDeclaredCommand(t *testing.T) {
	sd := &fakeSystemd{outcome: Outcome{OK: true}}
	h := newHelper(t, sd)

	resp := ask(t, h, `{"op":"run_step","project":"facturas","phase":"build","index":0,"deploy":"`+deployID+`"}`)
	if !resp.OK {
		t.Fatalf("respuesta = %+v", resp)
	}
	if len(sd.steps) != 1 {
		t.Fatalf("se corrieron %d pasos", len(sd.steps))
	}
	step := sd.steps[0]
	// Todo sale de la declaración: el pedido solo dijo qué paso.
	if step.Unit != "statussv-step-"+deployID+"-build-0.service" {
		t.Errorf("unidad = %q", step.Unit)
	}
	if step.User != "facturas" || step.Workdir != "/srv/facturas" || step.Timeout != 600*time.Second {
		t.Errorf("paso = %+v", step)
	}
	if got := strings.Join(step.Argv, " "); got != "/srv/facturas/.venv/bin/pip install -r requirements.txt" {
		t.Errorf("argv = %q", got)
	}
	if step.EnvFile == "" {
		t.Error("el paso no lleva el archivo de entorno declarado")
	}

	// El paso de otra fase.
	if resp := ask(t, h, `{"op":"run_step","project":"facturas","phase":"pre","index":0,"deploy":"`+deployID+`"}`); !resp.OK {
		t.Fatalf("pre = %+v", resp)
	}
	if got := sd.steps[1].Argv[0]; got != "/srv/facturas/.venv/bin/python" {
		t.Errorf("programa del paso pre = %q", got)
	}
}

func TestRunStepReportsHowItEnded(t *testing.T) {
	tests := map[string]struct {
		outcome  Outcome
		runErr   error
		wantCode Code
		wantExit int
	}{
		"código de salida": {outcome: Outcome{Result: "exit-code", ExitCode: 3}, wantCode: CodeStepFailed, wantExit: 3},
		"tiempo agotado":   {outcome: Outcome{Result: "timeout"}, wantCode: CodeTimeout},
		"señal":            {outcome: Outcome{Result: "signal"}, wantCode: CodeStepFailed},
		"systemd no anda":  {runErr: errors.New("conexión cerrada"), wantCode: CodeInternal},
		"systemd no contesta a tiempo": {
			runErr: context.DeadlineExceeded, wantCode: CodeTimeout,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHelper(t, &fakeSystemd{outcome: tt.outcome, runErr: tt.runErr})
			resp := ask(t, h, `{"op":"run_step","project":"facturas","phase":"build","index":1,"deploy":"`+deployID+`"}`)
			if resp.OK || resp.Code != tt.wantCode || resp.ExitCode != tt.wantExit {
				t.Errorf("respuesta = %+v, se esperaba %s con salida %d", resp, tt.wantCode, tt.wantExit)
			}
		})
	}
}

// Nada de lo que llega en un pedido se ejecuta: lo que no es un identificador
// conocido se rechaza sin tocar systemd.
func TestRejectedRequestsNeverReachSystemd(t *testing.T) {
	step := `"phase":"build","index":0,"deploy":"` + deployID + `"`
	tests := map[string]struct {
		request  string
		wantCode Code
	}{
		"no es JSON":                  {`run_step facturas build 0`, CodeInvalidRequest},
		"operación inventada":         {`{"op":"shell","project":"facturas"}`, CodeInvalidRequest},
		"sin operación":               {`{"project":"facturas"}`, CodeInvalidRequest},
		"con un comando":              {`{"op":"run_step","project":"facturas",` + step + `,"command":["/bin/sh"]}`, CodeInvalidRequest},
		"con un usuario":              {`{"op":"run_step","project":"facturas",` + step + `,"user":"root"}`, CodeInvalidRequest},
		"con una unidad":              {`{"op":"restart_units","project":"facturas","unit":"ssh.service"}`, CodeInvalidRequest},
		"dos pedidos":                 {`{"op":"check","project":"facturas"}{"op":"check","project":"facturas"}`, CodeInvalidRequest},
		"fase inventada":              {`{"op":"run_step","project":"facturas","phase":"shell","index":0,"deploy":"` + deployID + `"}`, CodeInvalidRequest},
		"paso negativo":               {`{"op":"run_step","project":"facturas","phase":"build","index":-1,"deploy":"` + deployID + `"}`, CodeInvalidRequest},
		"deploy que no es un UUID":    {`{"op":"run_step","project":"facturas","phase":"build","index":0,"deploy":"../../etc"}`, CodeInvalidRequest},
		"deploy en mayúsculas":        {`{"op":"run_step","project":"facturas","phase":"build","index":0,"deploy":"` + strings.ToUpper(deployID) + `"}`, CodeInvalidRequest},
		"reinicio con datos de paso":  {`{"op":"restart_units","project":"facturas",` + step + `}`, CodeInvalidRequest},
		"parada con una unidad":       {`{"op":"stop_units","project":"facturas","unit":"ssh.service"}`, CodeInvalidRequest},
		"parada con datos de paso":    {`{"op":"stop_units","project":"facturas",` + step + `}`, CodeInvalidRequest},
		"parada de uno de Docker":     {`{"op":"stop_units","project":"tienda"}`, CodeNotDeclared},
		"proyecto con barra":          {`{"op":"check","project":"../facturas"}`, CodeInvalidRequest},
		"pedido enorme":               {`{"op":"check","project":"` + strings.Repeat("a", maxRequestBytes) + `"}`, CodeInvalidRequest},
		"proyecto sin declarar":       {`{"op":"restart_units","project":"otro"}`, CodeNotDeclared},
		"proyecto de Docker":          {`{"op":"restart_units","project":"tienda"}`, CodeNotDeclared},
		"paso que no existe":          {`{"op":"run_step","project":"facturas","phase":"build","index":2,"deploy":"` + deployID + `"}`, CodeNoSuchStep},
		"fase sin pasos":              {`{"op":"run_step","project":"facturas","phase":"post","index":0,"deploy":"` + deployID + `"}`, CodeNoSuchStep},
		"parar un paso que no existe": {`{"op":"stop_step","project":"facturas","phase":"post","index":0,"deploy":"` + deployID + `"}`, CodeNoSuchStep},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			sd := &fakeSystemd{outcome: Outcome{OK: true}}
			resp := ask(t, newHelper(t, sd), tt.request)
			if resp.OK || resp.Code != tt.wantCode {
				t.Errorf("respuesta = %+v, se esperaba %s", resp, tt.wantCode)
			}
			if len(sd.steps)+len(sd.restarts)+len(sd.stopped) != 0 {
				t.Errorf("el pedido llegó a systemd: %+v %v %v", sd.steps, sd.restarts, sd.stopped)
			}
		})
	}
}

func TestRestartUnitsInDeclaredOrder(t *testing.T) {
	sd := &fakeSystemd{}
	h := newHelper(t, sd)
	if resp := ask(t, h, `{"op":"restart_units","project":"facturas"}`); !resp.OK {
		t.Fatalf("respuesta = %+v", resp)
	}
	if got := strings.Join(sd.restarts, " "); got != "gunicorn-facturas.service facturas-worker.service" {
		t.Errorf("reinicios = %q", got)
	}

	// Si una no reinicia, se frena ahí y dice cuál.
	sd = &fakeSystemd{failRestart: "gunicorn-facturas.service"}
	resp := ask(t, newHelper(t, sd), `{"op":"restart_units","project":"facturas"}`)
	if resp.OK || resp.Code != CodeRestartFailed || resp.Unit != "gunicorn-facturas.service" {
		t.Errorf("respuesta = %+v", resp)
	}
	if len(sd.restarts) != 1 {
		t.Errorf("siguió reiniciando después de la falla: %v", sd.restarts)
	}
}

// Un primer deploy que no quedó bien para lo que levantó (ADR-021): las
// unidades declaradas, al revés de como arrancan.
func TestStopUnitsInReverseOrder(t *testing.T) {
	sd := &fakeSystemd{}
	if resp := ask(t, newHelper(t, sd), `{"op":"stop_units","project":"facturas"}`); !resp.OK {
		t.Fatalf("respuesta = %+v", resp)
	}
	if got := strings.Join(sd.Stopped(), " "); got != "facturas-worker.service gunicorn-facturas.service" {
		t.Errorf("paradas = %q", got)
	}
	if len(sd.restarts) != 0 {
		t.Errorf("parar reinició algo: %v", sd.restarts)
	}

	// Si una no para, sigue con las demás y dice cuál quedó.
	sd = &fakeSystemd{failStop: "facturas-worker.service"}
	resp := ask(t, newHelper(t, sd), `{"op":"stop_units","project":"facturas"}`)
	if resp.OK || resp.Code != CodeStopFailed || resp.Unit != "facturas-worker.service" {
		t.Errorf("respuesta = %+v", resp)
	}
	if len(sd.Stopped()) != 2 {
		t.Errorf("no intentó parar las dos: %v", sd.Stopped())
	}
}

func TestStopStep(t *testing.T) {
	sd := &fakeSystemd{}
	resp := ask(t, newHelper(t, sd), `{"op":"stop_step","project":"facturas","phase":"build","index":1,"deploy":"`+deployID+`"}`)
	if !resp.OK {
		t.Fatalf("respuesta = %+v", resp)
	}
	if got := sd.Stopped(); len(got) != 1 || got[0] != "statussv-step-"+deployID+"-build-1.service" {
		t.Errorf("paradas = %v", got)
	}
}

func TestCheck(t *testing.T) {
	if resp := ask(t, newHelper(t, &fakeSystemd{}), `{"op":"check","project":"facturas"}`); !resp.OK {
		t.Fatalf("respuesta = %+v", resp)
	}

	t.Run("falta el usuario", func(t *testing.T) {
		h := newHelper(t, &fakeSystemd{})
		h.UserExists = func(string) bool { return false }
		if resp := ask(t, h, `{"op":"check","project":"facturas"}`); resp.Code != CodeNotReady || !strings.Contains(resp.Message, "run_as") {
			t.Errorf("respuesta = %+v", resp)
		}
	})
	t.Run("falta el archivo de entorno", func(t *testing.T) {
		h := newHelper(t, &fakeSystemd{})
		load := h.Load
		h.Load = func(dir, name string) (projectspec.Spec, error) {
			spec, err := load(dir, name)
			spec.EnvFile = filepath.Join(t.TempDir(), "no-existe")
			return spec, err
		}
		if resp := ask(t, h, `{"op":"check","project":"facturas"}`); resp.Code != CodeNotReady || !strings.Contains(resp.Message, "env_file") {
			t.Errorf("respuesta = %+v", resp)
		}
	})
	t.Run("systemd no conoce una unidad", func(t *testing.T) {
		h := newHelper(t, &fakeSystemd{missing: map[string]bool{"facturas-worker.service": true}})
		resp := ask(t, h, `{"op":"check","project":"facturas"}`)
		if resp.Code != CodeNotReady || resp.Unit != "facturas-worker.service" {
			t.Errorf("respuesta = %+v", resp)
		}
	})
}

// Si el agent se va en medio de un paso, el paso se para: no queda corriendo
// sin un deploy que lo espere.
func TestDisconnectStopsTheStep(t *testing.T) {
	sd := &fakeSystemd{gate: make(chan struct{})}
	h := newHelper(t, sd)

	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Serve(t.Context(), server)
	}()
	request := `{"op":"run_step","project":"facturas","phase":"build","index":0,"deploy":"` + deployID + `"}` + "\n"
	if _, err := client.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	// Se espera a que el paso esté corriendo, y se corta.
	deadline := time.Now().Add(5 * time.Second)
	for {
		sd.mu.Lock()
		running := len(sd.steps) == 1
		sd.mu.Unlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("el paso no arrancó")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = client.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("el ayudante no terminó después del corte")
	}
	if got := sd.Stopped(); len(got) != 1 || got[0] != "statussv-step-"+deployID+"-build-0.service" {
		t.Errorf("paradas = %v, se esperaba el paso que corría", got)
	}
}

func TestRequestTimesOut(t *testing.T) {
	// Un pedido que nunca llega no deja al ayudante esperando para siempre:
	// se prueba con una conexión que se cierra sin mandar nada.
	client, server := net.Pipe()
	h := newHelper(t, &fakeSystemd{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Serve(t.Context(), server)
	}()
	go func() { _, _ = io.Copy(io.Discard, client) }()
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("el ayudante quedó esperando un pedido")
	}
}

// check dice qué variables requeridas faltan en el archivo de entorno, que el
// agent no puede leer: solo los nombres (ADR-022).
func TestCheckRequiredEnv(t *testing.T) {
	withRequired := func(h *Helper, required ...string) {
		load := h.Load
		h.Load = func(dir, name string) (projectspec.Spec, error) {
			spec, err := load(dir, name)
			spec.RequiredEnv = required
			return spec, err
		}
	}

	h := newHelper(t, &fakeSystemd{})
	withRequired(h, "CLAVE", "SECRET_KEY", "DATABASE_URL")
	resp := ask(t, h, `{"op":"check","project":"facturas"}`)
	if resp.OK || resp.Code != CodeNotReady || !resp.EnvChecked || strings.Join(resp.Missing, ",") != "SECRET_KEY,DATABASE_URL" {
		t.Fatalf("respuesta = %+v", resp)
	}
	if strings.Contains(resp.Message, "valor") {
		t.Errorf("la respuesta muestra un valor: %+v", resp)
	}

	h = newHelper(t, &fakeSystemd{})
	withRequired(h, "CLAVE")
	if resp := ask(t, h, `{"op":"check","project":"facturas"}`); !resp.OK || !resp.EnvChecked || len(resp.Missing) != 0 {
		t.Errorf("con todo cargado = %+v", resp)
	}

	// Sin variables requeridas, no hay nada que mirar.
	if resp := ask(t, newHelper(t, &fakeSystemd{}), `{"op":"check","project":"facturas"}`); !resp.OK || resp.EnvChecked {
		t.Errorf("sin requeridas = %+v", resp)
	}

	// Aunque otra cosa no esté lista, los nombres que faltan viajan igual.
	h = newHelper(t, &fakeSystemd{missing: map[string]bool{"facturas-worker.service": true}})
	withRequired(h, "SECRET_KEY")
	if resp := ask(t, h, `{"op":"check","project":"facturas"}`); resp.Unit == "" || strings.Join(resp.Missing, ",") != "SECRET_KEY" {
		t.Errorf("con una unidad que falta = %+v", resp)
	}
}

func TestEnableUnits(t *testing.T) {
	sd := &fakeSystemd{}
	if resp := ask(t, newHelper(t, sd), `{"op":"enable_units","project":"facturas"}`); !resp.OK {
		t.Fatalf("respuesta = %+v", resp)
	}
	if strings.Join(sd.enabled, ",") != "gunicorn-facturas.service,facturas-worker.service" {
		t.Errorf("habilitadas = %v", sd.enabled)
	}
	if resp := ask(t, newHelper(t, &fakeSystemd{failEnable: true}), `{"op":"enable_units","project":"facturas"}`); resp.Code != CodeEnableFailed {
		t.Errorf("con falla = %+v", resp)
	}
	// Solo el proyecto: una unidad en el pedido es un pedido inválido.
	if resp := ask(t, newHelper(t, sd), `{"op":"enable_units","project":"facturas","unit":"sshd.service"}`); resp.Code != CodeInvalidRequest {
		t.Errorf("con una unidad en el pedido = %+v", resp)
	}
	if resp := ask(t, newHelper(t, sd), `{"op":"enable_units","project":"tienda"}`); resp.OK {
		t.Errorf("un proyecto de Docker = %+v", resp)
	}
}
