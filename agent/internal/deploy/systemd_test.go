package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	journald "github.com/JuanBaez77/status-sv/agent/internal/journal"
	"github.com/JuanBaez77/status-sv/agent/internal/projectspec"
	"github.com/JuanBaez77/status-sv/agent/internal/runner"
	sd "github.com/JuanBaez77/status-sv/agent/internal/systemd"
	"github.com/JuanBaez77/status-sv/agent/internal/unithelper"
	"github.com/JuanBaez77/status-sv/proto"
)

const (
	unitWeb    = "gunicorn-facturas.service"
	unitWorker = "facturas-worker.service"
)

// systemdSpec es un proyecto de systemd con dos pasos build, uno pre y uno
// post, y dos unidades.
func systemdSpec() projectspec.Spec {
	step := func(command ...string) projectspec.Step {
		return projectspec.Step{Command: command, Exec: "/srv/facturas/" + command[0], Timeout: time.Minute}
	}
	return projectspec.Spec{
		Project: proto.Project{
			Name: "facturas", Environment: proto.EnvProduction, Repo: "github.com/a/facturas", Branch: "main",
			Runtime: proto.RuntimeSystemd, Workdir: "/srv/facturas", Units: []string{unitWeb, unitWorker},
			Healthcheck: &proto.HealthcheckSpec{URL: "http://127.0.0.1:8000/health", ExpectStatus: 200, TimeoutSeconds: 5},
		},
		Build: []projectspec.Step{step(".venv/bin/pip", "install", "-r", "requirements.txt"), step("scripts/estaticos.sh")},
		Pre:   []projectspec.Step{step(".venv/bin/python", "manage.py", "migrate")},
		Post:  []projectspec.Step{step("scripts/aviso.sh")},
		RunAs: "facturas",
	}
}

// fakeHelper hace de ayudante root: anota lo que se le pide y falla donde el
// test le diga. broken dice si el código que está en el workdir anda mal.
type fakeHelper struct {
	mu    sync.Mutex
	calls []string
	// restarts cuenta los reinicios: las unidades cambian de proceso.
	restarts int

	checkErr error
	// failStep hace fallar un paso ("build 1") mientras broken sea verdadero.
	failStep   string
	stepErr    error
	restartErr error
	stopErr    error
	// stopped es que las unidades se pararon y no se arrancaron de nuevo.
	stopped bool
	broken  func() bool
	// gate, si no es nil, frena el primer paso hasta que se cierra o se cancela.
	gate chan struct{}
}

func (h *fakeHelper) call(name string) {
	h.mu.Lock()
	h.calls = append(h.calls, name)
	h.mu.Unlock()
}

func (h *fakeHelper) Calls() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.calls, " | ")
}

func (h *fakeHelper) Check(context.Context, string) error {
	h.call("check")
	return h.checkErr
}

func (h *fakeHelper) RunStep(ctx context.Context, _ string, phase unithelper.Phase, index int, _ uuid.UUID) error {
	name := fmt.Sprintf("%s %d", phase, index)
	h.call(name)
	if h.gate != nil {
		select {
		case <-h.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if name == h.failStep && h.broken() {
		return h.stepErr
	}
	return nil
}

func (h *fakeHelper) RestartUnits(context.Context, string) error {
	h.call("restart")
	if h.restartErr != nil && h.broken() {
		return h.restartErr
	}
	h.mu.Lock()
	h.restarts++
	h.stopped = false
	h.mu.Unlock()
	return nil
}

func (h *fakeHelper) StopUnits(context.Context, string) error {
	h.call("stop")
	if h.stopErr != nil {
		return h.stopErr
	}
	h.mu.Lock()
	h.stopped = true
	h.mu.Unlock()
	return nil
}

// fakeUnits contesta el estado de las unidades según cómo esté el código.
type fakeUnits struct {
	helper *fakeHelper
	// state arma el estado de una unidad; look cuenta las miradas.
	state func(unit string, look int) sd.UnitState
	mu    sync.Mutex
	looks int
}

func (u *fakeUnits) UnitState(_ context.Context, unit string) (sd.UnitState, error) {
	u.mu.Lock()
	u.looks++
	look := u.looks
	u.mu.Unlock()
	return u.state(unit, look), nil
}

// running es una unidad andando, con el proceso del último reinicio.
func (u *fakeUnits) running() sd.UnitState {
	u.helper.mu.Lock()
	defer u.helper.mu.Unlock()
	return sd.UnitState{Active: "active", Sub: "running", MainPID: uint32(1000 + u.helper.restarts)} //nolint:gosec // un contador chico
}

// fakeStepJournal devuelve lo que "escribió" cada unidad de paso.
type fakeStepJournal struct {
	mu    sync.Mutex
	lines map[string][]string
	asked []string
}

func (j *fakeStepJournal) Tail(_ context.Context, unit string, _ int) ([]journald.Entry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.asked = append(j.asked, unit)
	var entries []journald.Entry
	for i, line := range j.lines[unit] {
		// En el futuro, para que caigan después del arranque del paso.
		entries = append(entries, journald.Entry{TS: time.Now().Add(time.Hour + time.Duration(i)*time.Millisecond), Message: line})
	}
	return entries, nil
}

type okHealth struct{ passes func() bool }

func (h okHealth) Check(context.Context, proto.HealthcheckSpec) proto.HealthResult {
	if h.passes() {
		return proto.HealthResult{State: proto.HealthPassing, StatusCode: 200, LatencyMS: 9, CheckedAt: time.Now()}
	}
	return proto.HealthResult{State: proto.HealthFailing, StatusCode: 502, Detail: "respondió 502 y se esperaba 200", CheckedAt: time.Now()}
}

type systemdHarness struct {
	*harness
	helper  *fakeHelper
	units   *fakeUnits
	journal *fakeStepJournal
	// healthy dice si el healthcheck HTTP pasa.
	healthy func() bool
}

// newSystemdHarness arma un motor con un proyecto de systemd declarado
// (facturas) y sin Docker en el servidor. El commit nuevo anda bien salvo que
// el test diga otra cosa.
func newSystemdHarness(t *testing.T, stateDir string) *systemdHarness {
	t.Helper()
	if stateDir == "" {
		stateDir = t.TempDir()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	base := &harness{t: t, out: NewOutbox(), git: &fakeGit{head: commitOld}, compose: newFakeCompose(), state: stateDir, cancel: cancel}
	base.api = &fakeAPI{missing: map[string]bool{}}
	base.clock = &fakeClock{now: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	h := &systemdHarness{harness: base, journal: &fakeStepJournal{lines: map[string][]string{}}}
	h.healthy = func() bool { return true }

	// El código nuevo anda mal mientras el workdir esté en el commit nuevo.
	onNew := func() bool {
		head, _ := base.git.Head(context.Background())
		return head == commitNew
	}
	h.helper = &fakeHelper{broken: onNew}
	h.units = &fakeUnits{helper: h.helper}
	h.units.state = func(string, int) sd.UnitState { return h.units.running() }

	// Sin Docker: un servidor que tiene solo servicios de systemd.
	cfg := Config{Enabled: true, StateDir: stateDir, ProjectsDir: "/etc/status-sv/projects.d", Git: "/usr/bin/git"}
	load := func(_, name string) (projectspec.Spec, error) {
		if name != "facturas" {
			return projectspec.Spec{}, errors.New("no declarado")
		}
		return systemdSpec(), nil
	}
	engine, err := New(ctx, cfg, base.api, okHealth{passes: func() bool { return h.healthy() }}, load, base.out, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	engine.tools = fakeTools{git: base.git, compose: base.compose}
	engine.now = base.clock.Now
	engine.sleep = base.clock.Sleep
	engine.SetSystemd(Systemd{Helper: h.helper, Units: h.units, Journal: h.journal})
	base.engine = engine
	return h
}

func (h *systemdHarness) start(op proto.Operation) (uuid.UUID, proto.CommandAck) {
	h.t.Helper()
	id := uuid.Must(uuid.NewV7())
	cmd := proto.Command{Operation: op}
	if op == proto.OpDeploy {
		cmd.Deploy = &proto.DeployParams{Project: "facturas"}
	} else {
		cmd.Rollback = &proto.RollbackParams{Project: "facturas"}
	}
	return id, h.engine.Start(id, cmd)
}

func TestSystemdDeploySucceeds(t *testing.T) {
	h := newSystemdHarness(t, "")
	id := uuid.Must(uuid.NewV7())
	h.journal.lines[unithelper.StepUnit(id.String(), unithelper.PhaseBuild, 0)] = []string{
		"Collecting gunicorn==23.0.0", "usando PIP_PASSWORD=hunter2", "Successfully installed gunicorn-23.0.0",
	}
	ack := h.engine.Start(id, proto.Command{Operation: proto.OpDeploy, Deploy: &proto.DeployParams{Project: "facturas"}})
	if ack.Status != proto.CommandOK {
		t.Fatalf("acuse = %+v", ack)
	}
	ev := h.final(id)

	if ev.Status != proto.DeploySucceeded || ev.Code != "" {
		t.Fatalf("estado = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	want := "preflight:ok fetch:ok snapshot:ok checkout:ok build:ok build:ok pre:ok restart:ok healthcheck:ok post:ok"
	if got := stepNames(ev); got != want {
		t.Errorf("pasos =\n%s\nse esperaba\n%s", got, want)
	}
	// Todo lo del proyecto pasó por el ayudante, en orden; nada por Compose.
	if got := h.helper.Calls(); got != "check | build 0 | build 1 | pre 0 | restart | post 0" {
		t.Errorf("ayudante = %s", got)
	}
	if calls := h.compose.Calls(); len(calls) != 0 {
		t.Errorf("un proyecto de systemd llamó a Compose: %v", calls)
	}
	if h.git.head != commitNew {
		t.Errorf("el workdir quedó en %s", h.git.head)
	}

	// Cada paso declarado muestra su comando, y no lleva servicio de Compose.
	build := ev.Steps[4]
	if build.Detail != ".venv/bin/pip install -r requirements.txt" || build.Service != "" {
		t.Errorf("paso build = %+v", build)
	}
	// Su salida viene del diario, enmascarada.
	out := strings.Join(build.Output, "\n")
	if !strings.Contains(out, "Successfully installed") || strings.Contains(out, "hunter2") {
		t.Errorf("salida del build = %q", build.Output)
	}
	if detail := ev.Steps[2].Detail; detail != short(commitOld) {
		t.Errorf("snapshot = %q: un proyecto de systemd no anota imágenes", detail)
	}
	// El paso de reinicio dice qué reinició.
	if detail := ev.Steps[7].Detail; detail != unitWeb+", "+unitWorker {
		t.Errorf("reinicio = %q", detail)
	}
	if detail := ev.Steps[8].Detail; !strings.Contains(detail, "2 unidades andando") || !strings.Contains(detail, "healthcheck HTTP 200") {
		t.Errorf("verificación = %q", detail)
	}

	st, err := h.engine.store.loadState("facturas")
	if err != nil || len(st.History) != 2 || st.History[0].Commit != commitNew || st.History[1].Commit != commitOld {
		t.Fatalf("estados buenos = %+v, %v", st, err)
	}
}

// Un paso build que falla no llegó a reiniciar nada: vuelve el código,
// prepara de nuevo lo anterior y no toca el servicio.
func TestSystemdBuildFailureRollsBackWithoutRestarting(t *testing.T) {
	h := newSystemdHarness(t, "")
	h.helper.failStep = "build 1"
	h.helper.stepErr = &unithelper.Error{Code: unithelper.CodeStepFailed, ExitCode: 3, Message: "el paso terminó mal: exit-code"}

	id, _ := h.start(proto.OpDeploy)
	ev := h.final(id)

	if ev.Status != proto.DeployRolledBack || ev.Code != proto.DeployBuildFailed {
		t.Fatalf("estado = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if !strings.Contains(ev.Message, "scripts/estaticos.sh falló (salió con código 3)") {
		t.Errorf("mensaje = %q", ev.Message)
	}
	if got := stepNames(ev); got != "preflight:ok fetch:ok snapshot:ok checkout:ok build:ok build:failed rollback:ok" {
		t.Errorf("pasos = %s", got)
	}
	// Al volver atrás corre los dos build de nuevo, ya sobre el código anterior.
	if got := h.helper.Calls(); got != "check | build 0 | build 1 | build 0 | build 1" {
		t.Errorf("ayudante = %s", got)
	}
	if h.git.head != commitOld {
		t.Errorf("el workdir quedó en %s", h.git.head)
	}
	if detail := ev.Steps[len(ev.Steps)-1].Detail; !strings.Contains(detail, "no se había reiniciado") {
		t.Errorf("detalle del rollback = %q", detail)
	}
}

// El servicio no queda andando con el código nuevo: vuelve atrás, prepara lo
// anterior y reinicia de nuevo.
func TestSystemdUnhealthyRollsBack(t *testing.T) {
	h := newSystemdHarness(t, "")
	h.units.state = func(unit string, _ int) sd.UnitState {
		if unit == unitWeb && h.helper.broken() {
			return sd.UnitState{Active: "activating", Sub: "auto-restart"}
		}
		return h.units.running()
	}

	id, _ := h.start(proto.OpDeploy)
	ev := h.final(id)

	if ev.Status != proto.DeployRolledBack || ev.Code != proto.DeployUnhealthy {
		t.Fatalf("estado = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if !strings.Contains(ev.Message, unitWeb+": activating (auto-restart)") {
		t.Errorf("mensaje = %q", ev.Message)
	}
	if got := h.helper.Calls(); got != "check | build 0 | build 1 | pre 0 | restart | build 0 | build 1 | restart" {
		t.Errorf("ayudante = %s", got)
	}
	if h.git.head != commitOld {
		t.Errorf("el workdir quedó en %s", h.git.head)
	}
	if got := stepNames(ev); !strings.HasSuffix(got, "restart:ok healthcheck:failed rollback:ok") {
		t.Errorf("pasos = %s", got)
	}
}

// Un servicio que arranca, muere y systemd levanta de nuevo figura activo en
// cada mirada: lo que lo delata es que cambia de proceso.
func TestSystemdCrashLoopIsNotHealthy(t *testing.T) {
	h := newSystemdHarness(t, "")
	h.units.state = func(unit string, look int) sd.UnitState {
		state := h.units.running()
		if unit == unitWeb && h.helper.broken() {
			state.MainPID = uint32(5000 + look) //nolint:gosec // un contador chico
			state.Restarts = uint32(look)       //nolint:gosec // un contador chico
		}
		return state
	}

	id, _ := h.start(proto.OpDeploy)
	ev := h.final(id)

	if ev.Status != proto.DeployRolledBack || ev.Code != proto.DeployUnhealthy {
		t.Fatalf("estado = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if !strings.Contains(ev.Message, "se cayó y systemd la levantó de nuevo") {
		t.Errorf("mensaje = %q", ev.Message)
	}
}

// Sin healthcheck HTTP que pase, tampoco: las unidades solas no alcanzan si
// el proyecto declaró uno.
func TestSystemdHTTPHealthcheckCounts(t *testing.T) {
	h := newSystemdHarness(t, "")
	h.healthy = func() bool { return !h.helper.broken() }

	id, _ := h.start(proto.OpDeploy)
	ev := h.final(id)

	if ev.Status != proto.DeployRolledBack || ev.Code != proto.DeployUnhealthy || !strings.Contains(ev.Message, "healthcheck HTTP") {
		t.Fatalf("estado = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
}

func TestSystemdRestartFailureRollsBack(t *testing.T) {
	h := newSystemdHarness(t, "")
	h.helper.restartErr = &unithelper.Error{Code: unithelper.CodeRestartFailed, Unit: unitWorker, Message: "no se pudo reiniciar " + unitWorker}

	id, _ := h.start(proto.OpDeploy)
	ev := h.final(id)

	if ev.Status != proto.DeployRolledBack || ev.Code != proto.DeployRestartFailed {
		t.Fatalf("estado = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if !strings.Contains(ev.Message, "no se pudo reiniciar "+unitWorker) {
		t.Errorf("mensaje = %q", ev.Message)
	}
	// El reinicio se había empezado: al volver atrás se reinicia de nuevo.
	if got := h.helper.Calls(); !strings.HasSuffix(got, "restart | build 0 | build 1 | restart") {
		t.Errorf("ayudante = %s", got)
	}
}

func TestSystemdStepTimeout(t *testing.T) {
	h := newSystemdHarness(t, "")
	h.helper.failStep = "pre 0"
	h.helper.stepErr = &unithelper.Error{Code: unithelper.CodeTimeout, Message: "el paso se pasó de su tiempo"}

	id, _ := h.start(proto.OpDeploy)
	ev := h.final(id)

	if ev.Status != proto.DeployRolledBack || ev.Code != proto.DeployTimeout {
		t.Fatalf("estado = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if !strings.Contains(ev.Message, "manage.py migrate falló: se pasó de su tiempo") {
		t.Errorf("mensaje = %q", ev.Message)
	}
}

// Sin el ayudante, o con un proyecto que el ayudante dice que no está listo,
// el deploy falla antes de tocar nada.
func TestSystemdPreflightFailures(t *testing.T) {
	tests := map[string]struct {
		err  error
		want string
	}{
		"sin ayudante":             {unithelper.ErrUnavailable, "install-agent.sh --enable-deploy"},
		"usuario que no existe":    {&unithelper.Error{Code: unithelper.CodeNotReady, Message: "el usuario de run_as no existe en este servidor"}, "el usuario de run_as no existe"},
		"ayudante que no contesta": {errors.New("conexión rechazada"), "no contestó"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newSystemdHarness(t, "")
			h.helper.checkErr = tt.err
			id, _ := h.start(proto.OpDeploy)
			ev := h.final(id)

			if ev.Status != proto.DeployFailed || ev.Code != proto.DeployPreflightFailed || !strings.Contains(ev.Message, tt.want) {
				t.Fatalf("estado = %s %s: %s", ev.Status, ev.Code, ev.Message)
			}
			if len(h.git.checkouts) != 0 || h.helper.Calls() != "check" {
				t.Errorf("tocó algo: checkouts %v, ayudante %s", h.git.checkouts, h.helper.Calls())
			}
		})
	}
}

// El rollback manual de un proyecto de systemd corre build de nuevo: no hay
// imágenes guardadas a las que volver.
func TestSystemdManualRollback(t *testing.T) {
	h := newSystemdHarness(t, "")
	first, _ := h.start(proto.OpDeploy)
	if ev := h.final(first); ev.Status != proto.DeploySucceeded {
		t.Fatalf("deploy = %s %s", ev.Status, ev.Code)
	}
	h.out.Acked(first, 1000)
	h.helper.mu.Lock()
	h.helper.calls = nil
	h.helper.mu.Unlock()
	// El commit anterior anda bien.
	h.helper.broken = func() bool { return false }

	id, ack := h.start(proto.OpRollback)
	if ack.Status != proto.CommandOK {
		t.Fatalf("acuse = %+v", ack)
	}
	ev := h.final(id)
	if ev.Status != proto.DeploySucceeded || ev.Kind != proto.DeployKindRollback {
		t.Fatalf("rollback = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if got := stepNames(ev); got != "preflight:ok snapshot:ok checkout:ok build:ok build:ok restart:ok healthcheck:ok" {
		t.Errorf("pasos = %s", got)
	}
	// Sin pasos pre ni post: una migración no se deshace ni se repite.
	if got := h.helper.Calls(); got != "check | build 0 | build 1 | restart" {
		t.Errorf("ayudante = %s", got)
	}
	if h.git.head != commitOld || ev.Commit != commitOld {
		t.Errorf("quedó en %s, la foto dice %s", h.git.head, ev.Commit)
	}
	if detail := ev.Steps[5].Detail; detail != unitWeb+", "+unitWorker {
		t.Errorf("reinicio = %q", detail)
	}
}

// Si el agent se reinicia en medio de un paso, al arrancar vuelve atrás: lee
// la declaración de nuevo, porque el diario no guarda los pasos.
func TestSystemdRecoverAnInterruptedDeploy(t *testing.T) {
	dir := t.TempDir()
	first := newSystemdHarness(t, dir)
	first.helper.gate = make(chan struct{})
	id, _ := first.start(proto.OpDeploy)
	for !strings.Contains(first.helper.Calls(), "build 0") {
		time.Sleep(time.Millisecond)
	}
	first.cancel()
	first.engine.Wait()

	j, err := first.engine.store.loadJournal(id)
	if err != nil || j.Event.Status != proto.DeployRunning || !j.Touched || j.UpStarted {
		t.Fatalf("diario = %+v, %v", j, err)
	}

	second := newSystemdHarness(t, dir)
	second.git.head = commitNew
	second.engine.Recover()
	ev := second.final(id)
	if ev.Status != proto.DeployRolledBack || ev.Code != proto.DeployInterrupted {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if second.git.head != commitOld {
		t.Errorf("quedó en %s", second.git.head)
	}
	// Prepara lo anterior y no reinicia: el servicio no se había tocado.
	if got := second.helper.Calls(); got != "build 0 | build 1" {
		t.Errorf("ayudante = %s", got)
	}
}

func TestSystemdNeedsItsParts(t *testing.T) {
	h := newSystemdHarness(t, "")
	h.engine.systemd = nil
	if _, ack := h.start(proto.OpDeploy); ack.Status != proto.CommandRejected || ack.Code != proto.CommandUnavailable {
		t.Fatalf("acuse = %+v", ack)
	}
	// Con las piezas, un proyecto de systemd no necesita Docker en el servidor.
	h = newSystemdHarness(t, "")
	if h.engine.cfg.Docker != "" {
		t.Fatal("el test supone un servidor sin Docker")
	}
	if _, ack := h.start(proto.OpDeploy); ack.Status != proto.CommandOK {
		t.Fatalf("acuse = %+v", ack)
	}
	h.engine.Wait()
}

func TestJournalFollowerDoesNotRepeatLines(t *testing.T) {
	start := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	at := func(ms int, text string) journald.Entry {
		return journald.Entry{TS: start.Add(time.Duration(ms) * time.Millisecond), Message: text}
	}
	source := &scriptedJournal{}
	follow := &journalFollower{journal: source, unit: "x.service", after: start}
	tail := runner.NewTail(20, 200)

	// De antes del paso (el mismo paso, corrido al volver atrás): no entra.
	source.entries = []journald.Entry{at(-500, "de la corrida anterior"), at(10, "uno"), at(20, "dos")}
	follow.read(t.Context(), tail)
	// La lectura siguiente trae lo mismo y algo más, con dos renglones en el
	// mismo instante que el último ya visto.
	source.entries = []journald.Entry{at(10, "uno"), at(20, "dos"), at(20, "dos bis"), at(30, "tres")}
	follow.read(t.Context(), tail)
	follow.read(t.Context(), tail)

	if got := strings.Join(tail.Lines(), ","); got != "uno,dos,dos bis,tres" {
		t.Errorf("salida = %q", got)
	}
}

type scriptedJournal struct{ entries []journald.Entry }

func (s *scriptedJournal) Tail(context.Context, string, int) ([]journald.Entry, error) {
	return s.entries, nil
}
