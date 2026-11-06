package deploy

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

// restart pide un reinicio de tienda, de una unidad o de todo.
func (h *harness) restart(unit string) (uuid.UUID, proto.CommandAck) {
	h.t.Helper()
	id := uuid.Must(uuid.NewV7())
	return id, h.engine.Start(id, proto.Command{Operation: proto.OpRestart, Restart: &proto.RestartParams{Project: "tienda", Unit: unit}})
}

func TestRestartSucceedsWithoutTouchingCodeOrHistory(t *testing.T) {
	h := newHarness(t, "")
	// Antes, un deploy bueno: el reinicio informa su commit como el que corre.
	deployID, _ := h.start(proto.OpDeploy, "")
	if ev := h.final(deployID); ev.Status != proto.DeploySucceeded {
		t.Fatalf("deploy = %s %s", ev.Status, ev.Message)
	}
	before, err := h.engine.store.loadState("tienda")
	if err != nil {
		t.Fatal(err)
	}
	calls := len(h.compose.Calls())

	id, ack := h.restart("")
	if ack.Status != proto.CommandOK {
		t.Fatalf("acuse = %+v", ack)
	}
	ev := h.final(id)
	if ev.Status != proto.DeploySucceeded || ev.Kind != proto.DeployKindRestart || ev.Code != "" {
		t.Fatalf("reinicio = %s %s %s: %s", ev.Kind, ev.Status, ev.Code, ev.Message)
	}
	if got := stepNames(ev); got != "preflight:ok restart:ok healthcheck:ok" {
		t.Errorf("pasos = %s", got)
	}
	if ev.Commit != commitNew || ev.PreviousCommit != "" {
		t.Errorf("commits = %q ← %q", ev.Commit, ev.PreviousCommit)
	}
	if got := strings.Join(h.compose.Calls()[calls:], " | "); got != "restart" {
		t.Errorf("compose = %s: un reinicio no compila ni recrea", got)
	}
	if h.git.head != commitNew {
		t.Errorf("el workdir quedó en %s", h.git.head)
	}

	after, err := h.engine.store.loadState("tienda")
	if err != nil || len(after.History) != len(before.History) || after.History[0].Commit != before.History[0].Commit {
		t.Errorf("un reinicio no deja un estado bueno nuevo: antes %+v, después %+v (%v)", before.History, after.History, err)
	}
	if len(h.compose.pruned) != 1 {
		t.Errorf("un reinicio no suelta imágenes guardadas: %v", h.compose.pruned)
	}
}

func TestRestartOfAProjectNeverDeployed(t *testing.T) {
	h := newHarness(t, "")
	id, _ := h.restart("")
	ev := h.final(id)
	// Sin historia, no sabe qué commit corre: lo deja vacío.
	if ev.Status != proto.DeploySucceeded || ev.Commit != "" {
		t.Fatalf("reinicio = %s, commit %q", ev.Status, ev.Commit)
	}
}

func TestRestartOneService(t *testing.T) {
	h := newHarness(t, "")
	id, _ := h.restart("web")
	ev := h.final(id)
	if ev.Status != proto.DeploySucceeded {
		t.Fatalf("reinicio = %s %s", ev.Status, ev.Message)
	}
	if got := strings.Join(h.compose.Calls(), " | "); got != "restart web" {
		t.Errorf("compose = %s", got)
	}
	if ev.Steps[1].Detail != "web" {
		t.Errorf("el paso dice %q", ev.Steps[1].Detail)
	}
}

func TestRestartOfAServiceThatIsNotTheProjects(t *testing.T) {
	h := newHarness(t, "")
	id, _ := h.restart("postgres-de-otro")
	ev := h.final(id)
	if ev.Status != proto.DeployFailed || ev.Code != proto.DeployPreflightFailed {
		t.Fatalf("reinicio = %s %s", ev.Status, ev.Code)
	}
	if !strings.Contains(ev.Message, "no tiene la unidad postgres-de-otro; las suyas son web") {
		t.Errorf("mensaje = %q", ev.Message)
	}
	if calls := h.compose.Calls(); len(calls) != 0 {
		t.Errorf("no se reinició nada, y compose recibió %v", calls)
	}
}

func TestRestartOfAProjectWithNothingRunning(t *testing.T) {
	h := newHarness(t, "")
	h.compose.running = ""
	id, _ := h.restart("")
	if ev := h.final(id); ev.Status != proto.DeployFailed || ev.Code != proto.DeployPreflightFailed ||
		!strings.Contains(ev.Message, "no hay nada del proyecto corriendo") {
		t.Fatalf("reinicio = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
}

// Un reinicio que no queda bien termina en failed, sin vuelta atrás: no hay
// otra versión a la que volver.
func TestRestartThatDoesNotComeBackHealthyFailsWithoutRollingBack(t *testing.T) {
	h := newHarness(t, "")
	h.health.passes = func(string) bool { return false }
	id, _ := h.restart("")
	ev := h.final(id)
	if ev.Status != proto.DeployFailed || ev.Code != proto.DeployUnhealthy {
		t.Fatalf("reinicio = %s %s", ev.Status, ev.Code)
	}
	if got := stepNames(ev); got != "preflight:ok restart:ok healthcheck:failed" {
		t.Errorf("pasos = %s", got)
	}
	if got := strings.Join(h.compose.Calls(), " | "); got != "restart" {
		t.Errorf("compose = %s: no se vuelve atrás", got)
	}
}

func TestRestartThatFails(t *testing.T) {
	h := newHarness(t, "")
	h.compose.restartErr = errors.New("exit status 1")
	id, _ := h.restart("")
	if ev := h.final(id); ev.Status != proto.DeployFailed || ev.Code != proto.DeployRestartFailed {
		t.Fatalf("reinicio = %s %s", ev.Status, ev.Code)
	}
}

func TestRestartFollowsTheDeployRules(t *testing.T) {
	t.Run("apagado", func(t *testing.T) {
		h := newHarness(t, "")
		h.engine.cfg.Enabled = false
		if _, ack := h.restart(""); ack.Status != proto.CommandRejected || ack.Code != proto.CommandDisabled {
			t.Errorf("acuse = %+v", ack)
		}
	})
	t.Run("de a uno con los deploys", func(t *testing.T) {
		h := newHarness(t, "")
		h.compose.buildGate = make(chan struct{})
		deployID, _ := h.start(proto.OpDeploy, "")
		if _, ack := h.restart(""); ack.Status != proto.CommandRejected || ack.Code != proto.CommandBusy {
			t.Errorf("acuse = %+v", ack)
		}
		close(h.compose.buildGate)
		h.final(deployID)
	})
	t.Run("un proyecto que no está declarado", func(t *testing.T) {
		h := newHarness(t, "")
		id := uuid.Must(uuid.NewV7())
		ack := h.engine.Start(id, proto.Command{Operation: proto.OpRestart, Restart: &proto.RestartParams{Project: "otro"}})
		if ack.Status != proto.CommandRejected || ack.Code != proto.CommandUnknownTarget {
			t.Errorf("acuse = %+v", ack)
		}
	})
}

func TestSystemdRestart(t *testing.T) {
	t.Run("todas las unidades", func(t *testing.T) {
		h := newSystemdHarness(t, "")
		id := uuid.Must(uuid.NewV7())
		h.engine.Start(id, proto.Command{Operation: proto.OpRestart, Restart: &proto.RestartParams{Project: "facturas"}})
		ev := h.final(id)
		if ev.Status != proto.DeploySucceeded || stepNames(ev) != "preflight:ok restart:ok healthcheck:ok" {
			t.Fatalf("reinicio = %s: %s (%s)", ev.Status, stepNames(ev), ev.Message)
		}
		if got := h.helper.Calls(); got != "check | restart" {
			t.Errorf("ayudante = %s", got)
		}
	})
	t.Run("una unidad", func(t *testing.T) {
		h := newSystemdHarness(t, "")
		id := uuid.Must(uuid.NewV7())
		h.engine.Start(id, proto.Command{Operation: proto.OpRestart, Restart: &proto.RestartParams{Project: "facturas", Unit: unitWorker}})
		ev := h.final(id)
		if ev.Status != proto.DeploySucceeded {
			t.Fatalf("reinicio = %s %s", ev.Status, ev.Message)
		}
		if got := h.helper.Calls(); got != "check | restart "+unitWorker {
			t.Errorf("ayudante = %s", got)
		}
	})
	t.Run("una unidad que no es del proyecto", func(t *testing.T) {
		h := newSystemdHarness(t, "")
		id := uuid.Must(uuid.NewV7())
		h.engine.Start(id, proto.Command{Operation: proto.OpRestart, Restart: &proto.RestartParams{Project: "facturas", Unit: "nginx.service"}})
		ev := h.final(id)
		if ev.Status != proto.DeployFailed || ev.Code != proto.DeployPreflightFailed {
			t.Fatalf("reinicio = %s %s", ev.Status, ev.Code)
		}
		if got := h.helper.Calls(); got != "check" {
			t.Errorf("ayudante = %s: no se le pidió reiniciar nada", got)
		}
	})
}
