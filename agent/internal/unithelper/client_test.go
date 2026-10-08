package unithelper

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/internal/projectspec"
)

// listen levanta un ayudante de prueba en un socket unix y devuelve su ruta.
func listen(t *testing.T, h *Helper) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("el ayudante corre en Linux; los sockets unix de Windows no cierran igual")
	}
	// Un directorio corto: la ruta de un socket unix no pasa de 108 bytes.
	dir, err := os.MkdirTemp("", "uh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("escuchar: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Una conexión por pedido, como con la unidad de socket.
			go func() {
				h.Serve(context.Background(), conn)
				_ = conn.Close()
			}()
		}
	}()
	return socket
}

func TestClientAgainstHelper(t *testing.T) {
	sd := &fakeSystemd{outcome: Outcome{OK: true}}
	client := NewClient(listen(t, newHelper(t, sd)))
	deploy := uuid.MustParse(deployID)

	if err := client.Check(t.Context(), "facturas"); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if err := client.RunStep(t.Context(), "facturas", PhaseBuild, 0, deploy); err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if err := client.RestartUnits(t.Context(), "facturas"); err != nil {
		t.Fatalf("RestartUnits: %v", err)
	}
	if err := client.StopStep(t.Context(), "facturas", PhaseBuild, 0, deploy); err != nil {
		t.Fatalf("StopStep: %v", err)
	}
	if len(sd.steps) != 1 || len(sd.restarts) != 2 || len(sd.Stopped()) != 1 {
		t.Errorf("systemd vio %d pasos, %d reinicios y %d paradas", len(sd.steps), len(sd.restarts), len(sd.Stopped()))
	}
}

func TestClientReturnsTheHelperError(t *testing.T) {
	sd := &fakeSystemd{outcome: Outcome{Result: "exit-code", ExitCode: 3}, failRestart: "facturas-worker.service"}
	client := NewClient(listen(t, newHelper(t, sd)))

	err := client.RunStep(t.Context(), "facturas", PhaseBuild, 0, uuid.MustParse(deployID))
	var helperErr *Error
	if !errors.As(err, &helperErr) || helperErr.Code != CodeStepFailed || helperErr.ExitCode != 3 {
		t.Fatalf("RunStep = %v, se esperaba step_failed con salida 3", err)
	}

	err = client.RestartUnits(t.Context(), "facturas")
	if !errors.As(err, &helperErr) || helperErr.Code != CodeRestartFailed || helperErr.Unit != "facturas-worker.service" {
		t.Fatalf("RestartUnits = %v", err)
	}

	err = client.Check(t.Context(), "otro")
	if !errors.As(err, &helperErr) || helperErr.Code != CodeNotDeclared {
		t.Fatalf("Check de un proyecto sin declarar = %v", err)
	}
}

// Un pedido mal armado no llega a salir.
func TestClientValidatesBeforeSending(t *testing.T) {
	client := NewClient(filepath.Join(t.TempDir(), "no-hay-socket"))
	err := client.RunStep(t.Context(), "../facturas", PhaseBuild, 0, uuid.MustParse(deployID))
	var helperErr *Error
	if !errors.As(err, &helperErr) || helperErr.Code != CodeInvalidRequest {
		t.Errorf("RunStep = %v, se esperaba invalid_request", err)
	}
	err = client.RunStep(t.Context(), "facturas", PhaseBuild, 0, uuid.Nil)
	if !errors.As(err, &helperErr) || helperErr.Code != CodeInvalidRequest {
		t.Errorf("RunStep sin deploy = %v, se esperaba invalid_request", err)
	}
}

func TestClientWithoutHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("un socket que no existe da otro error en Windows; el agent corre en Linux")
	}
	client := NewClient(filepath.Join(t.TempDir(), "no-hay-socket"))
	if err := client.Check(t.Context(), "facturas"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Check = %v, se esperaba ErrUnavailable", err)
	}
}

// Cancelar el pedido cierra la conexión, y el ayudante para el paso.
func TestCancelStopsTheStep(t *testing.T) {
	sd := &fakeSystemd{gate: make(chan struct{})}
	client := NewClient(listen(t, newHelper(t, sd)))

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- client.RunStep(ctx, "facturas", PhaseBuild, 0, uuid.MustParse(deployID))
	}()

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
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("RunStep = %v, se esperaba context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunStep no volvió después de cancelar")
	}
	for len(sd.Stopped()) == 0 {
		if time.Now().After(deadline.Add(5 * time.Second)) {
			t.Fatal("el ayudante no paró el paso")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStepUnitName(t *testing.T) {
	if got := StepUnit(deployID, PhasePre, 2); got != "statussv-step-"+deployID+"-pre-2.service" {
		t.Errorf("StepUnit = %q", got)
	}
}

func TestStepProperties(t *testing.T) {
	step := Step{
		Unit: "statussv-step-x-build-0.service", Description: "paso", User: "facturas", Workdir: "/srv/facturas",
		Argv: []string{"/srv/facturas/.venv/bin/pip", "install", "-r", "requirements.txt"}, Timeout: 10 * time.Minute,
	}
	props := map[string]any{}
	for _, p := range stepProperties(step) {
		props[p.Name] = p.Value.Value()
	}
	if props["User"] != "facturas" || props["WorkingDirectory"] != "/srv/facturas" || props["Type"] != "oneshot" {
		t.Errorf("propiedades = %v", props)
	}
	if props["TimeoutStartUSec"] != uint64(600_000_000) {
		t.Errorf("TimeoutStartUSec = %v", props["TimeoutStartUSec"])
	}
	if props["NoNewPrivileges"] != true || props["PrivateTmp"] != true || props["ProtectSystem"] != "full" {
		t.Errorf("endurecimiento = %v", props)
	}
	// Sin archivo de entorno declarado, la propiedad no va.
	if _, ok := props["EnvironmentFiles"]; ok {
		t.Error("EnvironmentFiles sin archivo declarado")
	}
	if _, ok := props["ExecStart"]; !ok {
		t.Error("falta ExecStart")
	}

	step.EnvFile = "/etc/facturas/env"
	found := false
	for _, p := range stepProperties(step) {
		if p.Name == "EnvironmentFiles" {
			found = true
			if got := p.Value.Signature().String(); got != "a(sb)" {
				t.Errorf("EnvironmentFiles tiene la forma %s, systemd espera a(sb)", got)
			}
		}
	}
	if !found {
		t.Error("falta EnvironmentFiles con el archivo declarado")
	}
}

// CheckEnv devuelve los nombres que faltan aunque el chequeo dé not_ready.
func TestClientCheckEnv(t *testing.T) {
	h := newHelper(t, &fakeSystemd{})
	load := h.Load
	h.Load = func(dir, name string) (projectspec.Spec, error) {
		spec, err := load(dir, name)
		spec.RequiredEnv = []string{"CLAVE", "SECRET_KEY"}
		return spec, err
	}
	client := NewClient(listen(t, h))
	missing, checked, err := client.CheckEnv(t.Context(), "facturas")
	if err != nil || !checked || len(missing) != 1 || missing[0] != "SECRET_KEY" {
		t.Errorf("CheckEnv = %v, %v, %v", missing, checked, err)
	}
	// Check, en cambio, lo da por no listo, con los nombres en el error.
	var helperErr *Error
	if err := client.Check(t.Context(), "facturas"); !errors.As(err, &helperErr) || len(helperErr.Missing) != 1 {
		t.Errorf("Check = %v", err)
	}
}
