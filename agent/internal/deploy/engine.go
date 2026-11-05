// Package deploy es el motor de deploy de proyectos Docker (ADR-015) y de
// proyectos de systemd (ADR-019).
//
// Recibe un deploy o un rollback ya validado por el contrato, lo acepta o lo
// rechaza enseguida, y lo corre aparte, paso por paso: chequeo previo, fetch,
// foto de lo que corre, checkout, build, pasos declarados, up o reinicio,
// verificación y pasos posteriores. Si algo falla después de tocar lo que
// corre, vuelve atrás solo. El primer deploy de un proyecto (ADR-021) son los
// mismos pasos sobre un workdir sin clonar, sin foto de lo anterior: si falla
// después de levantar algo, lo baja. Cada cambio sale como una foto completa
// (deploy_event) y queda en un diario en disco, así un deploy sobrevive a un
// corte del canal y, si el agent se reinicia en el medio, se vuelve atrás al
// arrancar.
//
// Solo opera sobre proyectos declarados en el servidor, con git y docker
// compose ejecutados con argumentos fijos. Los pasos de un proyecto de systemd
// no los ejecuta el agent: se los pide al ayudante root, que corre lo que diga
// el project.yaml. No hay ningún camino por el que el control plane haga
// correr un comando propio.
package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/agent/internal/compose"
	"github.com/JuanBaez77/status-sv/agent/internal/gitops"
	"github.com/JuanBaez77/status-sv/agent/internal/runner"
	"github.com/JuanBaez77/status-sv/internal/projectspec"
	"github.com/JuanBaez77/status-sv/proto"
)

// Tiempos del motor (ADR-015).
const (
	// DeployTimeout acota un deploy entero.
	DeployTimeout = 45 * time.Minute
	// rollbackTimeout acota la vuelta atrás, que corre aunque el deploy se
	// haya pasado de su tiempo.
	rollbackTimeout = 10 * time.Minute
	// VerifyWindow es cuánto se espera a que el proyecto quede bien.
	VerifyWindow = 120 * time.Second
	// verifyInterval es cada cuánto se mira.
	verifyInterval = 2 * time.Second
	// verifyPasses son las veces seguidas que tiene que estar bien.
	verifyPasses = 3
	// rollbackVerifyWindow es la verificación corta después de volver atrás:
	// solo los contenedores.
	rollbackVerifyWindow = 60 * time.Second
	// progressEvery es cada cuánto sale una foto mientras corre un paso largo,
	// con los últimos renglones de su salida.
	progressEvery = 5 * time.Second
	// envelopeMargin es lo que se reserva del tope de un mensaje para el sobre.
	envelopeMargin = 2 << 10
)

// Config es lo que el motor necesita saber del servidor.
type Config struct {
	// Enabled es el interruptor del drop-in 20-deploy.conf: sin él, todo
	// pedido se rechaza con disabled.
	Enabled bool
	// StateDir es el directorio de estado del agent; el diario va en
	// StateDir/deploys.
	StateDir string
	// ProjectsDir es donde se declaran los proyectos.
	ProjectsDir string
	// KeysDir es donde están las deploy keys, una por proyecto.
	KeysDir string
	// KnownHosts es el archivo de host keys conocidos para SSH.
	KnownHosts string
	// Git y Docker son las rutas absolutas de los programas.
	Git    string
	Docker string
	// Socket es el socket de Docker.
	Socket string
	// AllowFileRepos acepta repos file:// (solo pruebas de punta a punta).
	AllowFileRepos bool
}

// Las piezas que usa el motor, como interfaces para poder probarlo sin git ni
// Docker. Las cumplen gitops.Repo y compose.Project.
type (
	gitRepo interface {
		Cloned() (bool, error)
		Init(ctx context.Context, tail *runner.Tail) error
		Head(ctx context.Context) (string, error)
		CheckClean(ctx context.Context) error
		Fetch(ctx context.Context, fetchURL, branch string, tail *runner.Tail) error
		Resolve(ctx context.Context, branch, commit string) (string, error)
		HasCommit(ctx context.Context, commit string) bool
		Checkout(ctx context.Context, branch, sha string, tail *runner.Tail) error
	}
	composeProject interface {
		Version(ctx context.Context) (string, error)
		Validate(ctx context.Context, tail *runner.Tail) error
		Build(ctx context.Context, tail *runner.Tail) error
		Up(ctx context.Context, tail *runner.Tail) error
		Down(ctx context.Context, tail *runner.Tail) error
		Restart(ctx context.Context, service string, tail *runner.Tail) error
		Run(ctx context.Context, step projectspec.Step, tail *runner.Tail) error
		Snapshot(ctx context.Context, api compose.ImageAPI) ([]compose.ServiceImage, error)
		Retain(ctx context.Context, api compose.ImageAPI, commit string, images []compose.ServiceImage) error
		Restore(ctx context.Context, api compose.ImageAPI, images []compose.ServiceImage) error
		Prune(ctx context.Context, api compose.ImageAPI, keep []string) error
		Check(ctx context.Context, api compose.ImageAPI) (compose.Status, error)
	}
	healthChecker interface {
		Check(ctx context.Context, spec proto.HealthcheckSpec) proto.HealthResult
	}
	// tools arma el repo y el proyecto de Compose de un proyecto declarado.
	tools interface {
		Git(p proto.Project, key string) (gitRepo, error)
		Compose(p proto.Project) (composeProject, error)
	}
	// specLoader lee la declaración de un proyecto del directorio.
	specLoader func(dir, name string) (projectspec.Spec, error)
)

// Engine corre los deploys de un agent, de a uno.
type Engine struct {
	cfg    Config
	api    compose.ImageAPI
	health healthChecker
	tools  tools
	load   specLoader
	out    *Outbox
	store  *store
	log    *slog.Logger
	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) error
	// systemd son las piezas del deploy de proyectos de systemd; nil si el
	// agent no las tiene (ADR-019).
	systemd *Systemd

	// root es el contexto del agent: un deploy sigue aunque se corte el canal
	// y solo se interrumpe si el agent se apaga.
	root context.Context

	mu sync.Mutex
	// busy es el deploy en curso, o uuid.Nil.
	busy uuid.UUID
	// recovering es que todavía se está revisando lo que quedó de antes.
	recovering bool
	wg         sync.WaitGroup
}

// New arma el motor. root es el contexto del agent: al cancelarse, el deploy
// en curso se corta y se retoma (volviendo atrás) al próximo arranque.
func New(root context.Context, cfg Config, api compose.ImageAPI, health healthChecker, load specLoader, out *Outbox, log *slog.Logger) (*Engine, error) {
	st, err := newStore(filepath.Join(cfg.StateDir, "deploys"))
	if err != nil {
		return nil, err
	}
	e := &Engine{
		cfg: cfg, api: api, health: health, tools: realTools{cfg: cfg}, load: load, out: out, store: st, log: log,
		now: time.Now, sleep: sleepCtx, root: root,
	}
	out.setOnAck(e.acked)
	return e, nil
}

// SetSystemd le da al motor las piezas para desplegar proyectos de systemd
// (ADR-019). Se llama antes de atender comandos; sin esto, esos proyectos se
// rechazan.
func (e *Engine) SetSystemd(deps Systemd) {
	e.systemd = &deps
}

// Wait espera a que termine lo que esté corriendo. Se usa al apagar y en los
// tests.
func (e *Engine) Wait() {
	e.wg.Wait()
}

// Start acepta o rechaza un deploy, un rollback, un primer deploy o un
// reinicio. Si lo acepta, lo corre aparte y contesta enseguida: el avance sale
// por la bandeja.
//
// El ID del comando es el del deploy. Un comando repetido (el control plane
// no recibió el acuse) se contesta igual, sin volver a ejecutarlo.
func (e *Engine) Start(id uuid.UUID, cmd proto.Command) proto.CommandAck {
	var (
		kind               = proto.DeployKindDeploy
		name, commit, unit string
	)
	switch {
	case cmd.Operation == proto.OpDeploy && cmd.Deploy != nil:
		name, commit = cmd.Deploy.Project, cmd.Deploy.Commit
	case cmd.Operation == proto.OpRollback && cmd.Rollback != nil:
		kind, name = proto.DeployKindRollback, cmd.Rollback.Project
	case cmd.Operation == proto.OpBootstrap && cmd.Bootstrap != nil:
		kind, name = proto.DeployKindBootstrap, cmd.Bootstrap.Project
	case cmd.Operation == proto.OpRestart && cmd.Restart != nil:
		kind, name, unit = proto.DeployKindRestart, cmd.Restart.Project, cmd.Restart.Unit
	default:
		return reject(id, proto.CommandUnknownOperation, "el motor de deploy no atiende esa operación")
	}

	if j, err := e.store.loadJournal(id); err == nil && j.Event.Project == name {
		return proto.CommandAck{CommandID: id, Status: proto.CommandOK, Message: "ya estaba aceptado"}
	}
	if !e.cfg.Enabled {
		return reject(id, proto.CommandDisabled, "el deploy está apagado en este servidor (STATUSSV_DEPLOY_ENABLED)")
	}
	if e.cfg.Git == "" {
		return reject(id, proto.CommandUnavailable, "falta git en este servidor")
	}

	spec, err := e.load(e.cfg.ProjectsDir, name)
	if err != nil {
		return reject(id, proto.CommandUnknownTarget, "el proyecto no está declarado en este servidor o su archivo es inválido")
	}
	// Cada runtime necesita lo suyo: Docker, o las piezas del deploy de
	// systemd (ADR-019). Un servidor puede tener uno solo de los dos.
	switch spec.Project.Runtime {
	case proto.RuntimeSystemd:
		if e.systemd == nil {
			return reject(id, proto.CommandUnavailable, "este agent no puede desplegar proyectos de systemd")
		}
	default:
		if e.cfg.Docker == "" {
			return reject(id, proto.CommandUnavailable, "falta docker en este servidor")
		}
	}

	e.mu.Lock()
	if e.recovering || e.busy != uuid.Nil {
		e.mu.Unlock()
		return reject(id, proto.CommandBusy, "el agent ya está desplegando; se despliega de a uno")
	}
	e.busy = id
	e.mu.Unlock()

	now := e.now().UTC().Truncate(time.Millisecond)
	j := &journal{
		Event:   proto.DeployEvent{DeployID: id, Seq: 1, Project: name, Kind: kind, Status: proto.DeployRunning, StartedAt: now},
		Project: spec.Project,
	}
	if err := e.store.saveJournal(j); err != nil {
		e.release(id)
		e.log.Error("no se pudo escribir el diario del deploy", "deploy", id, "error", err)
		return reject(id, proto.CommandInternal, "no se pudo escribir el diario del deploy")
	}
	e.out.Put(j.Event)

	e.wg.Go(func() {
		defer e.release(id)
		r := &run{e: e, j: j, spec: spec, commit: commit, unit: unit}
		r.execute()
	})
	return proto.CommandAck{CommandID: id, Status: proto.CommandOK, Message: "deploy aceptado"}
}

func (e *Engine) release(id uuid.UUID) {
	e.mu.Lock()
	if e.busy == id {
		e.busy = uuid.Nil
	}
	e.mu.Unlock()
}

// acked anota en el diario que el control plane confirmó la foto seq.
func (e *Engine) acked(id uuid.UUID, seq int) {
	j, err := e.store.ackJournal(id, seq)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		e.log.Warn("no se pudo anotar el acuse de un deploy", "deploy", id, "error", err)
		return
	}
	if j.Event.Status.Final() && seq >= j.Event.Seq {
		if err := e.store.trimJournals(); err != nil {
			e.log.Warn("no se pudieron borrar diarios viejos", "error", err)
		}
	}
}

// Recover revisa lo que quedó de antes del arranque: vuelve a mandar las fotos
// finales sin confirmar y, si un deploy quedó a medias porque el agent se
// reinició, lo vuelve atrás. Mientras tanto no se aceptan deploys nuevos.
func (e *Engine) Recover() {
	e.mu.Lock()
	e.recovering = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.recovering = false
		e.mu.Unlock()
	}()

	journals, err := e.store.journals()
	if err != nil {
		e.log.Error("no se pudieron leer los diarios de deploy", "error", err)
		return
	}
	for _, j := range journals {
		switch {
		case j.Event.Status == proto.DeployRunning:
			e.log.Warn("un deploy quedó a medias: el agent se reinició en el medio", "deploy", j.Event.DeployID, "proyecto", j.Event.Project)
			r := &run{e: e, j: j}
			r.interrupted()
		case j.AckedSeq < j.Event.Seq:
			e.out.Put(j.Event)
		}
	}
}

// realTools arma las piezas de verdad.
type realTools struct {
	cfg Config
}

func (t realTools) Git(p proto.Project, key string) (gitRepo, error) {
	return gitops.Open(gitops.Config{
		Git: t.cfg.Git, StateDir: t.cfg.StateDir, KnownHosts: t.cfg.KnownHosts, AllowFileRepos: t.cfg.AllowFileRepos,
	}, p.Workdir, key)
}

func (t realTools) Compose(p proto.Project) (composeProject, error) {
	return compose.New(compose.Config{Docker: t.cfg.Docker, Socket: t.cfg.Socket, StateDir: t.cfg.StateDir}, p)
}

// keyFor devuelve la deploy key del proyecto si existe, como archivo regular.
func (e *Engine) keyFor(name string) string {
	if e.cfg.KeysDir == "" {
		return ""
	}
	path := filepath.Join(e.cfg.KeysDir, name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
}

func reject(id uuid.UUID, code proto.CommandErrorCode, message string) proto.CommandAck {
	return proto.CommandAck{CommandID: id, Status: proto.CommandRejected, Code: code, Message: message}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// fit deja afuera los renglones de salida más viejos hasta que la foto entre
// en un mensaje: con salida común entra siempre, pero una salida llena de
// caracteres que JSON escapa podría no entrar.
func fit(ev proto.DeployEvent) proto.DeployEvent {
	for {
		raw, err := json.Marshal(ev)
		if err != nil || len(raw) <= proto.MaxMessageBytes-envelopeMargin {
			return ev
		}
		trimmed := false
		for i := range ev.Steps {
			if len(ev.Steps[i].Output) > 0 {
				ev.Steps[i].Output = ev.Steps[i].Output[1:]
				trimmed = true
			}
		}
		if !trimmed {
			return ev
		}
	}
}

// errDeploy es un error del deploy con su código del contrato y una
// explicación para el panel.
type errDeploy struct {
	code    proto.DeployErrorCode
	message string
}

func (e *errDeploy) Error() string { return e.message }

func deployError(code proto.DeployErrorCode, format string, args ...any) error {
	return &errDeploy{code: code, message: fmt.Sprintf(format, args...)}
}

// codeOf saca el código de un error del deploy; si no lo tiene, internal.
func codeOf(err error) (proto.DeployErrorCode, string) {
	var de *errDeploy
	if errors.As(err, &de) {
		return de.code, de.message
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, runner.ErrTimeout) {
		return proto.DeployTimeout, "se pasó del tiempo máximo"
	}
	return proto.DeployInternal, err.Error()
}
