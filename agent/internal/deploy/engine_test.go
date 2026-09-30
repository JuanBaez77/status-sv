package deploy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/agent/internal/compose"
	"github.com/JuanBaez77/status-sv/agent/internal/docker"
	"github.com/JuanBaez77/status-sv/agent/internal/gitops"
	"github.com/JuanBaez77/status-sv/agent/internal/projectspec"
	"github.com/JuanBaez77/status-sv/agent/internal/runner"
	"github.com/JuanBaez77/status-sv/proto"
)

const (
	commitOld = "1111111111111111111111111111111111111111"
	commitNew = "2222222222222222222222222222222222222222"
	commitOff = "3333333333333333333333333333333333333333"
	imageOld  = "sha256:old"
	imageNew  = "sha256:new"
)

// fakeGit es un repo en memoria: la punta de main es commitNew.
type fakeGit struct {
	mu        sync.Mutex
	head      string
	dirty     bool
	fetchErr  error
	checkouts []string
}

func (g *fakeGit) Head(context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.head, nil
}

func (g *fakeGit) CheckClean(context.Context) error {
	if g.dirty {
		return gitops.ErrDirty
	}
	return nil
}

func (g *fakeGit) Fetch(_ context.Context, url, _ string, tail *runner.Tail) error {
	tail.Add("From " + url)
	return g.fetchErr
}

func (g *fakeGit) Resolve(_ context.Context, _, commit string) (string, error) {
	switch commit {
	case "":
		return commitNew, nil
	case commitOld, commitNew:
		return commit, nil
	case commitOff:
		return "", gitops.ErrNotOnBranch
	default:
		return "", gitops.ErrUnknownCommit
	}
}

func (g *fakeGit) HasCommit(_ context.Context, commit string) bool {
	return commit == commitOld || commit == commitNew
}

func (g *fakeGit) Checkout(_ context.Context, _, sha string, _ *runner.Tail) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.head = sha
	g.checkouts = append(g.checkouts, sha)
	return nil
}

// fakeCompose es un proyecto con un servicio, web. ref es la imagen a la que
// apunta la referencia del compose; running, la que usa el contenedor.
type fakeCompose struct {
	mu       sync.Mutex
	ref      string
	running  string
	calls    []string
	buildErr error
	stepErr  error
	// ready dice si los contenedores están bien con la imagen que corre.
	ready func(running string) bool
	// buildGate, si no es nil, frena el build hasta que se cierra.
	buildGate chan struct{}
	retained  map[string]string
	pruned    [][]string
}

func newFakeCompose() *fakeCompose {
	return &fakeCompose{ref: imageOld, running: imageOld, ready: func(string) bool { return true }, retained: map[string]string{}}
}

func (c *fakeCompose) call(name string) {
	c.mu.Lock()
	c.calls = append(c.calls, name)
	c.mu.Unlock()
}

func (c *fakeCompose) Calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *fakeCompose) Version(context.Context) (string, error) { return "2.38.2", nil }

func (c *fakeCompose) Validate(context.Context, *runner.Tail) error { return nil }

func (c *fakeCompose) Build(ctx context.Context, tail *runner.Tail) error {
	c.call("build")
	if c.buildGate != nil {
		select {
		case <-c.buildGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	tail.Add("#1 building with DATABASE_PASSWORD=hunter2")
	if c.buildErr != nil {
		return c.buildErr
	}
	c.mu.Lock()
	c.ref = imageNew
	c.mu.Unlock()
	return nil
}

func (c *fakeCompose) Up(context.Context, *runner.Tail) error {
	c.call("up")
	c.mu.Lock()
	c.running = c.ref
	c.mu.Unlock()
	return nil
}

func (c *fakeCompose) Run(_ context.Context, s projectspec.Step, _ *runner.Tail) error {
	c.call("run " + s.Service + " " + strings.Join(s.Command, " "))
	return c.stepErr
}

func (c *fakeCompose) Snapshot(context.Context, compose.ImageAPI) ([]compose.ServiceImage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return []compose.ServiceImage{{Service: "web", ID: c.running, Ref: "tienda-web"}}, nil
}

func (c *fakeCompose) Retain(_ context.Context, _ compose.ImageAPI, commit string, images []compose.ServiceImage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, img := range images {
		c.retained[commit] = img.ID
	}
	return nil
}

func (c *fakeCompose) Restore(_ context.Context, _ compose.ImageAPI, images []compose.ServiceImage) error {
	c.call("restore")
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ref = images[0].ID
	return nil
}

func (c *fakeCompose) Prune(_ context.Context, _ compose.ImageAPI, keep []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruned = append(c.pruned, keep)
	return nil
}

func (c *fakeCompose) Check(context.Context, compose.ImageAPI) (compose.Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ready(c.running) {
		return compose.Status{Ready: true, Detail: "1 contenedores bien"}, nil
	}
	return compose.Status{Detail: "tienda-web-1: reiniciándose"}, nil
}

type fakeTools struct {
	git     *fakeGit
	compose *fakeCompose
}

func (t fakeTools) Git(proto.Project, string) (gitRepo, error)    { return t.git, nil }
func (t fakeTools) Compose(proto.Project) (composeProject, error) { return t.compose, nil }

// fakeAPI solo contesta si una imagen existe.
type fakeAPI struct {
	compose.ImageAPI
	missing map[string]bool
}

func (a fakeAPI) ImageExists(_ context.Context, ref string) (bool, error) {
	return !a.missing[ref], nil
}

var _ compose.ImageAPI = fakeAPI{}
var _ = docker.Container{}

// fakeHealth pasa o no según la imagen que corre.
type fakeHealth struct {
	compose *fakeCompose
	passes  func(running string) bool
}

func (h fakeHealth) Check(context.Context, proto.HealthcheckSpec) proto.HealthResult {
	h.compose.mu.Lock()
	running := h.compose.running
	h.compose.mu.Unlock()
	if h.passes(running) {
		return proto.HealthResult{State: proto.HealthPassing, StatusCode: 200, LatencyMS: 12, CheckedAt: time.Now()}
	}
	return proto.HealthResult{State: proto.HealthFailing, StatusCode: 502, Detail: "respondió 502 y se esperaba 200", CheckedAt: time.Now()}
}

func spec() projectspec.Spec {
	return projectspec.Spec{
		Project: proto.Project{
			Name: "tienda", Environment: proto.EnvProduction, Repo: "github.com/a/tienda", Branch: "main",
			Runtime: proto.RuntimeDocker, Workdir: "/srv/tienda", ComposeFile: "compose.yml", ComposeProject: "tienda",
			Healthcheck: &proto.HealthcheckSpec{URL: "https://tienda.example.com/health", ExpectStatus: 200, TimeoutSeconds: 5},
		},
		Pre:  []projectspec.Step{{Service: "web", Command: []string{"python", "manage.py", "migrate"}, Timeout: time.Minute}},
		Post: []projectspec.Step{{Service: "web", Command: []string{"python", "manage.py", "warmup"}, Timeout: time.Minute}},
	}
}

type harness struct {
	t       *testing.T
	engine  *Engine
	out     *Outbox
	git     *fakeGit
	compose *fakeCompose
	health  *fakeHealth
	api     *fakeAPI
	state   string
	clock   *fakeClock
	cancel  context.CancelFunc
}

// fakeClock avanza solo cuando el motor duerme.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

func newHarness(t *testing.T, stateDir string) *harness {
	t.Helper()
	if stateDir == "" {
		stateDir = t.TempDir()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h := &harness{t: t, out: NewOutbox(), git: &fakeGit{head: commitOld}, compose: newFakeCompose(), state: stateDir, cancel: cancel}
	h.health = &fakeHealth{compose: h.compose, passes: func(string) bool { return true }}
	h.api = &fakeAPI{missing: map[string]bool{}}
	h.clock = &fakeClock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}

	cfg := Config{Enabled: true, StateDir: stateDir, ProjectsDir: "/etc/status-sv/projects.d"}
	load := func(_, name string) (projectspec.Spec, error) {
		if name != "tienda" {
			return projectspec.Spec{}, errors.New("no declarado")
		}
		return spec(), nil
	}
	engine, err := New(ctx, cfg, h.api, h.health, load, h.out, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	engine.tools = fakeTools{git: h.git, compose: h.compose}
	engine.now = h.clock.Now
	engine.sleep = h.clock.Sleep
	h.engine = engine
	return h
}

func (h *harness) start(op proto.Operation, commit string) (uuid.UUID, proto.CommandAck) {
	h.t.Helper()
	id := uuid.Must(uuid.NewV7())
	cmd := proto.Command{Operation: op}
	if op == proto.OpDeploy {
		cmd.Deploy = &proto.DeployParams{Project: "tienda", Commit: commit}
	} else {
		cmd.Rollback = &proto.RollbackParams{Project: "tienda"}
	}
	return id, h.engine.Start(id, cmd)
}

// final corre el deploy hasta el final y devuelve su última foto.
func (h *harness) final(id uuid.UUID) proto.DeployEvent {
	h.t.Helper()
	h.engine.Wait()
	for {
		ev, ok := h.out.Next(time.Now())
		if !ok {
			h.t.Fatalf("no quedó foto del deploy %s", id)
		}
		if ev.DeployID == id {
			if err := ev.Validate(); err != nil {
				h.t.Fatalf("foto final inválida: %v", err)
			}
			return ev
		}
		h.out.Acked(ev.DeployID, ev.Seq)
	}
}

func stepNames(ev proto.DeployEvent) string {
	var names []string
	for _, s := range ev.Steps {
		name := string(s.Name) + ":" + string(s.Status)
		names = append(names, name)
	}
	return strings.Join(names, " ")
}

func TestDeploySucceeds(t *testing.T) {
	h := newHarness(t, "")
	id, ack := h.start(proto.OpDeploy, "")
	if ack.Status != proto.CommandOK {
		t.Fatalf("acuse = %+v", ack)
	}
	ev := h.final(id)

	if ev.Status != proto.DeploySucceeded || ev.Code != "" {
		t.Fatalf("estado = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if ev.Commit != commitNew || ev.PreviousCommit != commitOld {
		t.Errorf("commits = %s ← %s", ev.Commit, ev.PreviousCommit)
	}
	want := "preflight:ok fetch:ok snapshot:ok checkout:ok build:ok pre:ok up:ok healthcheck:ok post:ok"
	if got := stepNames(ev); got != want {
		t.Errorf("pasos =\n%s\nse esperaba\n%s", got, want)
	}
	if got := strings.Join(h.compose.Calls(), " | "); got != "build | run web python manage.py migrate | up | run web python manage.py warmup" {
		t.Errorf("compose = %s", got)
	}
	// Lo que corría antes quedó guardado para volver.
	if h.compose.retained[commitOld] != imageOld {
		t.Errorf("no se guardó la imagen anterior: %v", h.compose.retained)
	}
	// La salida sale enmascarada.
	build := ev.Steps[4]
	if len(build.Output) == 0 || strings.Contains(strings.Join(build.Output, " "), "hunter2") {
		t.Errorf("salida del build = %q", build.Output)
	}
	st, err := h.engine.store.loadState("tienda")
	if err != nil || len(st.History) != 2 || st.History[0].Commit != commitNew || st.History[1].Commit != commitOld {
		t.Fatalf("estados buenos = %+v, %v", st, err)
	}
	if st.History[0].Images[0].ID != imageNew || st.History[1].Images[0].ID != imageOld {
		t.Errorf("imágenes anotadas = %+v", st.History)
	}
	if len(h.compose.pruned) != 1 || strings.Join(h.compose.pruned[0], ",") != commitNew+","+commitOld {
		t.Errorf("prune = %v", h.compose.pruned)
	}
}

func TestDeployOfAnExactCommit(t *testing.T) {
	h := newHarness(t, "")
	id, _ := h.start(proto.OpDeploy, commitOld)
	if ev := h.final(id); ev.Status != proto.DeploySucceeded || ev.Commit != commitOld {
		t.Fatalf("foto = %s %s", ev.Status, ev.Commit)
	}
}

func TestRejections(t *testing.T) {
	t.Run("apagado", func(t *testing.T) {
		h := newHarness(t, "")
		h.engine.cfg.Enabled = false
		if _, ack := h.start(proto.OpDeploy, ""); ack.Status != proto.CommandRejected || ack.Code != proto.CommandDisabled {
			t.Fatalf("acuse = %+v", ack)
		}
	})
	t.Run("proyecto no declarado", func(t *testing.T) {
		h := newHarness(t, "")
		id := uuid.Must(uuid.NewV7())
		ack := h.engine.Start(id, proto.Command{Operation: proto.OpDeploy, Deploy: &proto.DeployParams{Project: "otro"}})
		if ack.Status != proto.CommandRejected || ack.Code != proto.CommandUnknownTarget {
			t.Fatalf("acuse = %+v", ack)
		}
	})
	t.Run("get_logs no es del motor", func(t *testing.T) {
		h := newHarness(t, "")
		ack := h.engine.Start(uuid.Must(uuid.NewV7()), proto.Command{Operation: proto.OpGetLogs})
		if ack.Code != proto.CommandUnknownOperation {
			t.Fatalf("acuse = %+v", ack)
		}
	})
}

func TestOneDeployAtATimeAndIdempotency(t *testing.T) {
	h := newHarness(t, "")
	h.compose.buildGate = make(chan struct{})
	id, ack := h.start(proto.OpDeploy, "")
	if ack.Status != proto.CommandOK {
		t.Fatalf("acuse = %+v", ack)
	}

	// Mientras compila, otro pedido se rechaza.
	if _, other := h.start(proto.OpDeploy, ""); other.Code != proto.CommandBusy {
		t.Errorf("segundo pedido = %+v", other)
	}
	// El mismo comando repetido se contesta igual, sin correr otra vez.
	again := h.engine.Start(id, proto.Command{Operation: proto.OpDeploy, Deploy: &proto.DeployParams{Project: "tienda"}})
	if again.Status != proto.CommandOK {
		t.Errorf("repetido = %+v", again)
	}

	close(h.compose.buildGate)
	if ev := h.final(id); ev.Status != proto.DeploySucceeded {
		t.Fatalf("estado = %s", ev.Status)
	}
	if n := strings.Count(strings.Join(h.compose.Calls(), " "), "build"); n != 1 {
		t.Errorf("compiló %d veces", n)
	}
	// Terminado, se puede desplegar de nuevo.
	if _, ack := h.start(proto.OpDeploy, ""); ack.Status != proto.CommandOK {
		t.Errorf("después de terminar = %+v", ack)
	}
	h.engine.Wait()
}

func TestFailuresBeforeTouchingAnything(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(h *harness)
		commit string
		code   proto.DeployErrorCode
		steps  string
	}{
		{name: "árbol sucio", setup: func(h *harness) { h.git.dirty = true }, code: proto.DeployDirtyWorktree, steps: "preflight:failed"},
		{name: "fetch", setup: func(h *harness) { h.git.fetchErr = &runner.ExitError{Code: 128} }, code: proto.DeployFetchFailed, steps: "preflight:ok fetch:failed"},
		{name: "commit fuera de la rama", setup: func(*harness) {}, commit: commitOff, code: proto.DeployCommitNotOnBranch, steps: "preflight:ok fetch:failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, "")
			tt.setup(h)
			id, _ := h.start(proto.OpDeploy, tt.commit)
			ev := h.final(id)
			if ev.Status != proto.DeployFailed || ev.Code != tt.code {
				t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
			}
			if got := stepNames(ev); got != tt.steps {
				t.Errorf("pasos = %s", got)
			}
			if len(h.git.checkouts) != 0 || len(h.compose.Calls()) != 0 {
				t.Errorf("tocó algo: checkouts %v, compose %v", h.git.checkouts, h.compose.Calls())
			}
		})
	}
}

func TestBuildFailureRollsBackWithoutRecreating(t *testing.T) {
	h := newHarness(t, "")
	h.compose.buildErr = &runner.ExitError{Code: 1}
	id, _ := h.start(proto.OpDeploy, "")
	ev := h.final(id)

	if ev.Status != proto.DeployRolledBack || ev.Code != proto.DeployBuildFailed {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if got := stepNames(ev); got != "preflight:ok fetch:ok snapshot:ok checkout:ok build:failed rollback:ok" {
		t.Errorf("pasos = %s", got)
	}
	if got := strings.Join(h.compose.Calls(), " "); got != "build restore" {
		t.Errorf("compose = %s (no tendría que recrear contenedores)", got)
	}
	if h.git.head != commitOld {
		t.Errorf("el código quedó en %s", h.git.head)
	}
}

func TestUnhealthyRollsBack(t *testing.T) {
	h := newHarness(t, "")
	h.health.passes = func(running string) bool { return running == imageOld }
	id, _ := h.start(proto.OpDeploy, "")
	ev := h.final(id)

	if ev.Status != proto.DeployRolledBack || ev.Code != proto.DeployUnhealthy {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if !strings.Contains(ev.Message, "502") {
		t.Errorf("mensaje = %s", ev.Message)
	}
	if h.compose.running != imageOld || h.git.head != commitOld {
		t.Errorf("quedó corriendo %s en %s", h.compose.running, h.git.head)
	}
	if got := strings.Join(h.compose.Calls(), " "); !strings.HasSuffix(got, "up restore up") {
		t.Errorf("compose = %s", got)
	}
	// Un deploy que volvió atrás no cambia los estados buenos.
	if st, _ := h.engine.store.loadState("tienda"); len(st.History) != 0 {
		t.Errorf("estados buenos = %+v", st)
	}
}

func TestRollbackThatAlsoFails(t *testing.T) {
	h := newHarness(t, "")
	h.health.passes = func(string) bool { return false }
	h.compose.ready = func(string) bool { return false }
	id, _ := h.start(proto.OpDeploy, "")
	ev := h.final(id)

	if ev.Status != proto.DeployFailed || ev.Code != proto.DeployRollbackFailed {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if !strings.Contains(ev.Message, "no pudo volver atrás") {
		t.Errorf("mensaje = %s", ev.Message)
	}
}

func TestPreStepFailureRollsBack(t *testing.T) {
	h := newHarness(t, "")
	h.compose.stepErr = &runner.ExitError{Code: 2}
	id, _ := h.start(proto.OpDeploy, "")
	ev := h.final(id)
	if ev.Status != proto.DeployRolledBack || ev.Code != proto.DeployStepFailed {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if pre := ev.Steps[5]; pre.Name != proto.StepPre || pre.Service != "web" || !strings.Contains(pre.Detail, "código 2") {
		t.Errorf("paso pre = %+v", pre)
	}
	if strings.Contains(strings.Join(h.compose.Calls(), " "), "up") {
		t.Errorf("recreó contenedores: %v", h.compose.Calls())
	}
}

func TestManualRollback(t *testing.T) {
	h := newHarness(t, "")
	id, _ := h.start(proto.OpDeploy, "")
	if ev := h.final(id); ev.Status != proto.DeploySucceeded {
		t.Fatal(ev.Status)
	}

	back, ack := h.start(proto.OpRollback, "")
	if ack.Status != proto.CommandOK {
		t.Fatalf("acuse = %+v", ack)
	}
	ev := h.final(back)
	if ev.Status != proto.DeploySucceeded || ev.Kind != proto.DeployKindRollback {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if ev.Commit != commitOld || ev.PreviousCommit != commitNew {
		t.Errorf("commits = %s ← %s", ev.Commit, ev.PreviousCommit)
	}
	if got := stepNames(ev); got != "preflight:ok snapshot:ok checkout:ok build:skipped up:ok healthcheck:ok" {
		t.Errorf("pasos = %s", got)
	}
	if h.compose.running != imageOld || h.git.head != commitOld {
		t.Errorf("quedó corriendo %s en %s", h.compose.running, h.git.head)
	}
	// Otro rollback vuelve al que se desplegó: la historia se da vuelta.
	st, _ := h.engine.store.loadState("tienda")
	if st.History[0].Commit != commitOld || st.History[1].Commit != commitNew {
		t.Errorf("estados buenos = %+v", st.History)
	}
}

func TestManualRollbackWithoutPrevious(t *testing.T) {
	h := newHarness(t, "")
	id, _ := h.start(proto.OpRollback, "")
	ev := h.final(id)
	if ev.Status != proto.DeployFailed || ev.Code != proto.DeployNoPrevious {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
}

func TestManualRollbackWithAPrunedImage(t *testing.T) {
	h := newHarness(t, "")
	id, _ := h.start(proto.OpDeploy, "")
	h.final(id)
	h.api.missing[imageOld] = true

	back, _ := h.start(proto.OpRollback, "")
	ev := h.final(back)
	if ev.Status != proto.DeployFailed || ev.Code != proto.DeployMissingImage {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if h.git.head != commitNew {
		t.Errorf("tocó el código: %s", h.git.head)
	}
}

// Si el agent se reinicia a mitad de un deploy, al arrancar vuelve atrás.
func TestRecoverAnInterruptedDeploy(t *testing.T) {
	dir := t.TempDir()
	first := newHarness(t, dir)
	first.compose.buildGate = make(chan struct{})
	id, _ := first.start(proto.OpDeploy, "")
	// El agent se apaga mientras compila.
	for !strings.Contains(strings.Join(first.compose.Calls(), " "), "build") {
		time.Sleep(time.Millisecond)
	}
	first.cancel()
	first.engine.Wait()

	j, err := first.engine.store.loadJournal(id)
	if err != nil || j.Event.Status != proto.DeployRunning || !j.Touched {
		t.Fatalf("diario = %+v, %v", j, err)
	}

	second := newHarness(t, dir)
	second.git.head = commitNew
	second.compose.ref = imageNew
	second.engine.Recover()
	ev := second.final(id)
	if ev.Status != proto.DeployRolledBack || ev.Code != proto.DeployInterrupted {
		t.Fatalf("foto = %s %s: %s", ev.Status, ev.Code, ev.Message)
	}
	if second.git.head != commitOld || second.compose.ref != imageOld {
		t.Errorf("quedó en %s con %s", second.git.head, second.compose.ref)
	}
	if !strings.Contains(stepNames(ev), "build:failed") {
		t.Errorf("pasos = %s", stepNames(ev))
	}
}

// Una foto final sin confirmar se vuelve a mandar después de reiniciar; con
// el acuse queda anotada.
func TestRecoverResendsUnackedFinals(t *testing.T) {
	dir := t.TempDir()
	first := newHarness(t, dir)
	id, _ := first.start(proto.OpDeploy, "")
	first.engine.Wait()

	second := newHarness(t, dir)
	second.engine.Recover()
	ev, ok := second.out.Next(time.Now())
	if !ok || ev.DeployID != id || ev.Status != proto.DeploySucceeded {
		t.Fatalf("no se reenvió la foto final: %+v", ev)
	}
	second.out.Sent(uuid.Must(uuid.NewV7()), ev, time.Now())
	second.out.Acked(id, ev.Seq)
	j, _ := second.engine.store.loadJournal(id)
	if j.AckedSeq != ev.Seq {
		t.Errorf("acked_seq = %d", j.AckedSeq)
	}

	third := newHarness(t, dir)
	third.engine.Recover()
	if third.out.Len() != 0 {
		t.Error("se reenvió una foto ya confirmada")
	}
}

func TestFitTrimsTheOldestOutput(t *testing.T) {
	ev := proto.DeployEvent{DeployID: uuid.Must(uuid.NewV7()), Seq: 1, Project: "x", Kind: proto.DeployKindDeploy, Status: proto.DeployRunning, StartedAt: time.Now()}
	line := strings.Repeat("<", proto.MaxStepOutputLineLen)
	for range proto.MaxDeploySteps {
		out := make([]string, proto.MaxStepOutputLines)
		for i := range out {
			out[i] = line
		}
		ev.Steps = append(ev.Steps, proto.DeployStep{Name: proto.StepBuild, Status: proto.StepOK, Output: out})
	}
	fitted := fit(ev)
	raw, err := proto.Marshal(fitted)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > proto.MaxMessageBytes {
		t.Fatalf("ocupa %d bytes", len(raw))
	}
	if len(fitted.Steps[0].Output) == proto.MaxStepOutputLines {
		t.Error("no recortó nada")
	}
}
