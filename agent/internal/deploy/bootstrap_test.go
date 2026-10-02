package deploy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/agent/internal/runner"
	sd "github.com/JuanBaez77/status-sv/agent/internal/systemd"
	"github.com/JuanBaez77/status-sv/agent/internal/unithelper"
	"github.com/JuanBaez77/status-sv/proto"
)

// newFreshHarness es un servidor donde el proyecto está declarado y nunca se
// desplegó: el workdir existe sin clonar y no hay contenedores.
func newFreshHarness(t *testing.T, stateDir string) *harness {
	t.Helper()
	h := newHarness(t, stateDir)
	h.git.head = ""
	h.compose.ref, h.compose.running = "", ""
	return h
}

func (h *harness) bootstrap() (uuid.UUID, proto.CommandAck) {
	h.t.Helper()
	id := uuid.Must(uuid.NewV7())
	return id, h.engine.Start(id, proto.Command{Operation: proto.OpBootstrap, Bootstrap: &proto.BootstrapParams{Project: "tienda"}})
}

func TestBootstrapSucceeds(t *testing.T) {
	h := newFreshHarness(t, "")
	id, ack := h.bootstrap()
	if ack.Status != proto.CommandOK {
		t.Fatalf("acuse = %+v", ack)
	}
	ev := h.final(id)

	if ev.Status != proto.DeploySucceeded || ev.Kind != proto.DeployKindBootstrap {
		t.Fatalf("foto = %s %s %s: %s", ev.Kind, ev.Status, ev.Code, ev.Message)
	}
	// No hay nada anterior: ni foto ni commit.
	if ev.Commit != commitNew || ev.PreviousCommit != "" {
		t.Errorf("commits = %s ← %q", ev.Commit, ev.PreviousCommit)
	}
	want := "preflight:ok fetch:ok checkout:ok build:ok pre:ok up:ok healthcheck:ok post:ok"
	if got := stepNames(ev); got != want {
		t.Errorf("pasos =\n%s\nse esperaba\n%s", got, want)
	}
	if detail := ev.Steps[0].Detail; !strings.Contains(detail, "workdir sin clonar") || !strings.Contains(detail, "compose 2.38.2") {
		t.Errorf("chequeo previo = %q", detail)
	}
	if h.git.inits != 1 || h.git.head != commitNew {
		t.Errorf("repositorio: %d inicios, en %q", h.git.inits, h.git.head)
	}
	if got := strings.Join(h.compose.Calls(), " | "); got != "build | run web python manage.py migrate | up | run web python manage.py warmup" {
		t.Errorf("compose = %s", got)
	}

	// Queda anotado como el primer estado bueno: desde acá, son deploys.
	st, err := h.engine.store.loadState("tienda")
	if err != nil || len(st.History) != 1 || st.History[0].Commit != commitNew || st.History[0].Images[0].ID != imageNew {
		t.Fatalf("estados buenos = %+v, %v", st, err)
	}
	again, _ := h.bootstrap()
	if ev := h.final(again); ev.Status != proto.DeployFailed || ev.Code != proto.DeployAlreadyDeployed {
		t.Fatalf("segundo primer deploy = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
}

// Un primer deploy no se hace sobre un proyecto que ya corre ni sin el
// workdir que tiene que dejar una persona. No toca nada.
func TestBootstrapRefusals(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(h *harness)
		code    proto.DeployErrorCode
		message string
	}{
		{
			name:    "ya tiene contenedores",
			setup:   func(h *harness) { h.compose.running = imageOld },
			code:    proto.DeployAlreadyDeployed,
			message: "contenedores de web",
		},
		{
			name:    "sin workdir",
			setup:   func(h *harness) { h.git.missing = true },
			code:    proto.DeployPreflightFailed,
			message: "lo crea una persona",
		},
		{
			name: "el clon de un intento anterior, cambiado a mano",
			setup: func(h *harness) {
				h.git.head, h.git.dirty = commitOld, true
			},
			code:    proto.DeployDirtyWorktree,
			message: "cambiados a mano",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFreshHarness(t, "")
			tt.setup(h)
			id, _ := h.bootstrap()
			ev := h.final(id)
			if ev.Status != proto.DeployFailed || ev.Code != tt.code || !strings.Contains(ev.Message, tt.message) {
				t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
			}
			if got := stepNames(ev); got != "preflight:failed" {
				t.Errorf("pasos = %s", got)
			}
			if h.git.inits != 0 || len(h.git.checkouts) != 0 || len(h.compose.Calls()) != 0 {
				t.Errorf("tocó algo: %d inicios, checkouts %v, compose %v", h.git.inits, h.git.checkouts, h.compose.Calls())
			}
		})
	}
}

// Si falla antes de levantar nada, no hay nada que bajar: el código queda
// clonado y el intento siguiente lo acepta.
func TestBootstrapBuildFailureLeavesTheClone(t *testing.T) {
	h := newFreshHarness(t, "")
	h.compose.buildErr = &runner.ExitError{Code: 1}
	id, _ := h.bootstrap()
	ev := h.final(id)

	if ev.Status != proto.DeployFailed || ev.Code != proto.DeployBuildFailed {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if got := stepNames(ev); got != "preflight:ok fetch:ok checkout:ok build:failed" {
		t.Errorf("pasos = %s", got)
	}
	if got := strings.Join(h.compose.Calls(), " "); got != "build" {
		t.Errorf("compose = %s", got)
	}
	if h.git.head != commitNew {
		t.Errorf("el código clonado no quedó: %q", h.git.head)
	}

	h.compose.buildErr = nil
	again, _ := h.bootstrap()
	ev = h.final(again)
	if ev.Status != proto.DeploySucceeded {
		t.Fatalf("segundo intento = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if detail := ev.Steps[0].Detail; !strings.Contains(detail, "intento anterior") {
		t.Errorf("chequeo previo = %q", detail)
	}
}

// Si no queda bien después de levantarlo, baja lo que levantó. No hay un
// estado anterior al que volver.
func TestBootstrapUnhealthyBringsItDown(t *testing.T) {
	h := newFreshHarness(t, "")
	h.health.passes = func(string) bool { return false }
	id, _ := h.bootstrap()
	ev := h.final(id)

	if ev.Status != proto.DeployRolledBack || ev.Code != proto.DeployUnhealthy {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if got := stepNames(ev); got != "preflight:ok fetch:ok checkout:ok build:ok pre:ok up:ok healthcheck:failed rollback:ok" {
		t.Errorf("pasos = %s", got)
	}
	last := ev.Steps[len(ev.Steps)-1]
	if !strings.Contains(last.Detail, "bajó lo que había levantado") {
		t.Errorf("paso de vuelta atrás = %q", last.Detail)
	}
	if got := strings.Join(h.compose.Calls(), " "); !strings.HasSuffix(got, "up down") {
		t.Errorf("compose = %s", got)
	}
	if h.compose.running != "" {
		t.Errorf("quedó corriendo %s", h.compose.running)
	}
	// El código no se borra, y no se anota ningún estado bueno.
	if h.git.head != commitNew {
		t.Errorf("el código clonado no quedó: %q", h.git.head)
	}
	if st, _ := h.engine.store.loadState("tienda"); len(st.History) != 0 {
		t.Errorf("estados buenos = %+v", st)
	}
}

func TestBootstrapThatCannotBringItDown(t *testing.T) {
	h := newFreshHarness(t, "")
	h.health.passes = func(string) bool { return false }
	h.compose.downErr = &runner.ExitError{Code: 1}
	id, _ := h.bootstrap()
	ev := h.final(id)

	if ev.Status != proto.DeployFailed || ev.Code != proto.DeployRollbackFailed {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if !strings.Contains(ev.Message, "no pudo bajar lo que había levantado") {
		t.Errorf("mensaje = %s", ev.Message)
	}
}

// Si el agent se reinicia con el proyecto ya levantado y sin verificar, al
// arrancar lo baja.
func TestRecoverAnInterruptedBootstrap(t *testing.T) {
	dir := t.TempDir()
	first := newFreshHarness(t, dir)
	// El healthcheck no pasa nunca: el deploy queda en la ventana de
	// verificación hasta que el agent se apaga.
	first.health.passes = func(string) bool { return false }
	first.engine.sleep = func(ctx context.Context, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}
	id, _ := first.bootstrap()
	for !strings.Contains(strings.Join(first.compose.Calls(), " "), "up") {
		time.Sleep(time.Millisecond)
	}
	first.cancel()
	first.engine.Wait()

	j, err := first.engine.store.loadJournal(id)
	if err != nil || j.Event.Status != proto.DeployRunning || !j.UpStarted || j.Before != nil {
		t.Fatalf("diario = %+v, %v", j, err)
	}

	second := newFreshHarness(t, dir)
	second.git.head = commitNew
	second.compose.ref, second.compose.running = imageNew, imageNew
	second.engine.Recover()
	ev := second.final(id)
	if ev.Status != proto.DeployRolledBack || ev.Code != proto.DeployInterrupted {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if got := strings.Join(second.compose.Calls(), " "); got != "down" {
		t.Errorf("compose = %s", got)
	}
	if second.compose.running != "" || second.git.head != commitNew {
		t.Errorf("quedó corriendo %q en %q", second.compose.running, second.git.head)
	}
}

// Si se reinicia antes de levantar nada, no hay nada que bajar.
func TestRecoverABootstrapInterruptedWhileBuilding(t *testing.T) {
	dir := t.TempDir()
	first := newFreshHarness(t, dir)
	first.compose.buildGate = make(chan struct{})
	id, _ := first.bootstrap()
	for !strings.Contains(strings.Join(first.compose.Calls(), " "), "build") {
		time.Sleep(time.Millisecond)
	}
	first.cancel()
	first.engine.Wait()

	second := newFreshHarness(t, dir)
	second.git.head = commitNew
	second.engine.Recover()
	ev := second.final(id)
	if ev.Status != proto.DeployFailed || ev.Code != proto.DeployInterrupted {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if len(second.compose.Calls()) != 0 {
		t.Errorf("compose = %v", second.compose.Calls())
	}
}

// Un deploy común sobre un proyecto que nunca se clonó dice qué es lo que va.
func TestDeployOfAProjectThatWasNeverCloned(t *testing.T) {
	h := newFreshHarness(t, "")
	id, _ := h.start(proto.OpDeploy, "")
	ev := h.final(id)
	if ev.Status != proto.DeployFailed || ev.Code != proto.DeployPreflightFailed || !strings.Contains(ev.Message, "primer deploy") {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if h.git.inits != 0 {
		t.Error("un deploy común inició el repositorio")
	}
}

// newFreshSystemdHarness es un servidor con un proyecto de systemd declarado
// que nunca se desplegó: el workdir sin clonar y las unidades paradas hasta
// que el ayudante las arranque.
func newFreshSystemdHarness(t *testing.T) *systemdHarness {
	t.Helper()
	h := newSystemdHarness(t, "")
	h.git.head = ""
	h.units.state = func(string, int) sd.UnitState {
		h.helper.mu.Lock()
		up := h.helper.restarts > 0 && !h.helper.stopped
		h.helper.mu.Unlock()
		if !up {
			return sd.UnitState{Active: "inactive", Sub: "dead"}
		}
		return h.units.running()
	}
	return h
}

func (h *systemdHarness) bootstrap() uuid.UUID {
	h.t.Helper()
	id := uuid.Must(uuid.NewV7())
	ack := h.engine.Start(id, proto.Command{Operation: proto.OpBootstrap, Bootstrap: &proto.BootstrapParams{Project: "facturas"}})
	if ack.Status != proto.CommandOK {
		h.t.Fatalf("acuse = %+v", ack)
	}
	return id
}

func TestSystemdBootstrapSucceeds(t *testing.T) {
	h := newFreshSystemdHarness(t)
	ev := h.final(h.bootstrap())

	if ev.Status != proto.DeploySucceeded || ev.Kind != proto.DeployKindBootstrap || ev.PreviousCommit != "" {
		t.Fatalf("foto = %s %s %s: %s", ev.Kind, ev.Status, ev.Code, ev.Message)
	}
	want := "preflight:ok fetch:ok checkout:ok build:ok build:ok pre:ok restart:ok healthcheck:ok post:ok"
	if got := stepNames(ev); got != want {
		t.Errorf("pasos =\n%s\nse esperaba\n%s", got, want)
	}
	// El ayudante revisa el proyecto antes de clonar y otra vez con el código
	// en el workdir; después, lo de un deploy.
	if got := h.helper.Calls(); got != "check | check | build 0 | build 1 | pre 0 | restart | post 0" {
		t.Errorf("ayudante = %s", got)
	}
	if h.git.inits != 1 || h.git.head != commitNew {
		t.Errorf("repositorio: %d inicios, en %q", h.git.inits, h.git.head)
	}
	st, err := h.engine.store.loadState("facturas")
	if err != nil || len(st.History) != 1 || st.History[0].Commit != commitNew {
		t.Fatalf("estados buenos = %+v, %v", st, err)
	}
}

// Con una unidad del proyecto andando no es un primer deploy.
func TestSystemdBootstrapRefusesARunningProject(t *testing.T) {
	h := newSystemdHarness(t, "")
	h.git.head = ""
	ev := h.final(h.bootstrap())
	if ev.Status != proto.DeployFailed || ev.Code != proto.DeployAlreadyDeployed || !strings.Contains(ev.Message, unitWeb+" está active") {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if got := h.helper.Calls(); got != "" {
		t.Errorf("ayudante = %s", got)
	}
}

// Lo que falta en el servidor se dice antes de clonar nada.
func TestSystemdBootstrapChecksTheServerFirst(t *testing.T) {
	h := newFreshSystemdHarness(t)
	h.helper.checkErr = &unithelper.Error{Code: unithelper.CodeNotReady, Message: "el usuario de run_as no existe en este servidor"}
	ev := h.final(h.bootstrap())
	if ev.Status != proto.DeployFailed || ev.Code != proto.DeployPreflightFailed || !strings.Contains(ev.Message, "run_as") {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if h.git.inits != 0 {
		t.Error("inició el repositorio sin que el servidor estuviera listo")
	}
}

// Si el servicio no queda bien, el ayudante para las unidades.
func TestSystemdBootstrapUnhealthyStopsTheUnits(t *testing.T) {
	h := newFreshSystemdHarness(t)
	h.healthy = func() bool { return false }
	ev := h.final(h.bootstrap())

	if ev.Status != proto.DeployRolledBack || ev.Code != proto.DeployUnhealthy {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if got := h.helper.Calls(); !strings.HasSuffix(got, "restart | stop") {
		t.Errorf("ayudante = %s", got)
	}
	last := ev.Steps[len(ev.Steps)-1]
	if last.Name != proto.StepRollback || last.Status != proto.StepOK || !strings.Contains(strings.Join(last.Output, "\n"), "parando "+unitWeb) {
		t.Errorf("paso de vuelta atrás = %+v", last)
	}
	if st, _ := h.engine.store.loadState("facturas"); len(st.History) != 0 {
		t.Errorf("estados buenos = %+v", st)
	}

	// Si tampoco se pueden parar, hay que mirarlo.
	h = newFreshSystemdHarness(t)
	h.healthy = func() bool { return false }
	h.helper.stopErr = &unithelper.Error{Code: unithelper.CodeStopFailed, Unit: unitWorker, Message: "no se pudo parar " + unitWorker}
	ev = h.final(h.bootstrap())
	if ev.Status != proto.DeployFailed || ev.Code != proto.DeployRollbackFailed || !strings.Contains(ev.Message, "no se pudo parar "+unitWorker) {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
}
