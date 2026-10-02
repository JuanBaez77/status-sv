package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/gitops"
	"github.com/JuanBaez77/status-sv/agent/internal/projectspec"
	"github.com/JuanBaez77/status-sv/agent/internal/runner"
	"github.com/JuanBaez77/status-sv/proto"
)

// run es un deploy o un rollback en curso.
//
// El diario lo escriben dos goroutines: la que corre los pasos y la que manda
// el avance de un paso largo. Por eso todo cambio al diario pasa por update y
// toda foto por emit, con el candado tomado.
type run struct {
	e    *Engine
	j    *journal
	spec projectspec.Spec
	// commit es el pedido; vacío es la punta de la rama.
	commit string

	git gitRepo
	rt  runtime
	// target es a dónde va un rollback manual.
	target *GoodState

	mu sync.Mutex
}

// update cambia el diario con el candado tomado.
func (r *run) update(fn func(j *journal)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r.j)
}

// execute corre el deploy entero y deja la foto final.
func (r *run) execute() {
	ctx, cancel := context.WithTimeout(r.e.root, DeployTimeout)
	defer cancel()

	var err error
	if r.j.Event.Kind == proto.DeployKindRollback {
		err = r.rollbackSteps(ctx)
	} else {
		err = r.deploySteps(ctx)
	}
	if err == nil {
		r.succeed()
		return
	}
	if r.e.root.Err() != nil {
		// El agent se está apagando: el diario queda en curso y al arrancar
		// se vuelve atrás (Recover). Acá no se hace nada a medias.
		r.e.log.Warn("el agent se apaga en medio de un deploy; se retoma al arrancar", "deploy", r.j.Event.DeployID)
		return
	}
	r.fail(err)
}

// deploySteps son los pasos de un deploy (ADR-015, punto 4).
func (r *run) deploySteps(ctx context.Context) error {
	if err := r.step(ctx, proto.StepPreflight, "", r.preflight); err != nil {
		return err
	}
	if err := r.step(ctx, proto.StepFetch, "", r.fetch); err != nil {
		return err
	}
	if err := r.step(ctx, proto.StepSnapshot, "", r.snapshot); err != nil {
		return err
	}
	if err := r.step(ctx, proto.StepCheckout, "", r.checkout(func() string { return r.j.Event.Commit })); err != nil {
		return err
	}
	if err := r.step(ctx, proto.StepBuild, "", func(ctx context.Context, tail *runner.Tail) (string, error) {
		return "", r.rt.Build(ctx, tail)
	}); err != nil {
		return err
	}
	for _, s := range r.spec.Pre {
		if err := r.step(ctx, proto.StepPre, s.Service, r.declared(s)); err != nil {
			return err
		}
	}
	if err := r.step(ctx, proto.StepUp, "", r.up); err != nil {
		return err
	}
	if err := r.step(ctx, proto.StepHealthcheck, "", r.verify); err != nil {
		return err
	}
	for _, s := range r.spec.Post {
		if err := r.step(ctx, proto.StepPost, s.Service, r.declared(s)); err != nil {
			return err
		}
	}
	return nil
}

// rollbackSteps son los pasos de un rollback manual: a donde estaba el
// proyecto antes del último deploy bueno, sin compilar ni pasos declarados.
func (r *run) rollbackSteps(ctx context.Context) error {
	if err := r.step(ctx, proto.StepPreflight, "", r.preflight); err != nil {
		return err
	}
	if err := r.step(ctx, proto.StepSnapshot, "", r.rollbackTarget); err != nil {
		return err
	}
	if err := r.step(ctx, proto.StepCheckout, "", r.checkout(func() string { return r.target.Commit })); err != nil {
		return err
	}
	r.skip(proto.StepBuild, "un rollback no compila: usa las imágenes guardadas")
	if err := r.step(ctx, proto.StepUp, "", func(ctx context.Context, tail *runner.Tail) (string, error) {
		if err := r.rt.Restore(ctx, r.target.Images); err != nil {
			return "", err
		}
		return r.up(ctx, tail)
	}); err != nil {
		return err
	}
	return r.step(ctx, proto.StepHealthcheck, "", r.verify)
}

// rollbackTarget elige a dónde vuelve un rollback manual, revisa que se
// pueda, y anota y guarda lo que corre ahora por si hay que volver de nuevo.
func (r *run) rollbackTarget(ctx context.Context, tail *runner.Tail) (string, error) {
	st, err := r.e.store.loadState(r.j.Project.Name)
	if err != nil {
		return "", err
	}
	if len(st.History) < 2 {
		return "", deployError(proto.DeployNoPrevious, "no hay un estado anterior anotado a donde volver")
	}
	target := st.History[1]
	if !r.git.HasCommit(ctx, target.Commit) {
		return "", deployError(proto.DeployNoPrevious, "el commit anterior %s ya no está en el repositorio", short(target.Commit))
	}
	if err := r.rt.CanRestore(ctx, target.Images); err != nil {
		return "", err
	}
	r.target = &target
	r.update(func(j *journal) { j.Event.Commit = target.Commit })
	return r.snapshot(ctx, tail)
}

// preflight revisa todo lo que puede fallar antes de tocar nada.
func (r *run) preflight(ctx context.Context, _ *runner.Tail) (string, error) {
	p := r.j.Project
	var err error
	if r.git, err = r.e.tools.Git(p, r.e.keyFor(p.Name)); err != nil {
		return "", deployError(proto.DeployPreflightFailed, "%v", err)
	}
	if r.rt, err = r.e.runtimeFor(p); err != nil {
		return "", deployError(proto.DeployPreflightFailed, "%v", err)
	}
	if _, err := r.git.Head(ctx); err != nil {
		if errors.Is(err, gitops.ErrNotRepository) {
			return "", deployError(proto.DeployPreflightFailed, "%s no es un repositorio de git (o el agent no lo puede leer)", p.Workdir)
		}
		return "", deployError(proto.DeployPreflightFailed, "no se pudo leer el repositorio: %v", err)
	}
	if err := r.git.CheckClean(ctx); err != nil {
		if errors.Is(err, gitops.ErrDirty) {
			return "", deployError(proto.DeployDirtyWorktree, "hay archivos versionados cambiados a mano en %s; el deploy no los pisa", p.Workdir)
		}
		return "", deployError(proto.DeployPreflightFailed, "%v", err)
	}
	return r.rt.Preflight(ctx)
}

// fetch trae la rama declarada y resuelve el commit que se despliega.
func (r *run) fetch(ctx context.Context, tail *runner.Tail) (string, error) {
	p := r.j.Project
	url, err := gitops.FetchURL(p.Repo, r.e.keyFor(p.Name) != "", r.e.cfg.AllowFileRepos)
	if err != nil {
		return "", deployError(proto.DeployFetchFailed, "%v", err)
	}
	if err := r.git.Fetch(ctx, url, p.Branch, tail); err != nil {
		return "", stepFailure(err, proto.DeployFetchFailed, "no se pudo traer la rama "+p.Branch)
	}
	sha, err := r.git.Resolve(ctx, p.Branch, r.commit)
	switch {
	case errors.Is(err, gitops.ErrNotOnBranch):
		return "", deployError(proto.DeployCommitNotOnBranch, "el commit %s no está en la rama %s", short(r.commit), p.Branch)
	case errors.Is(err, gitops.ErrUnknownCommit):
		return "", deployError(proto.DeployCommitNotOnBranch, "el commit %s no existe en el repositorio", short(r.commit))
	case err != nil:
		return "", deployError(proto.DeployFetchFailed, "%v", err)
	}
	r.update(func(j *journal) { j.Event.Commit = sha })
	return p.Branch + " en " + short(sha), nil
}

// snapshot anota cómo está el proyecto antes de tocarlo y guarda sus imágenes
// para poder volver. Queda en el diario antes del checkout.
func (r *run) snapshot(ctx context.Context, _ *runner.Tail) (string, error) {
	head, err := r.git.Head(ctx)
	if err != nil {
		return "", err
	}
	images, err := r.rt.Snapshot(ctx)
	if err != nil {
		return "", fmt.Errorf("anotar las imágenes: %w", err)
	}
	if err := r.rt.Retain(ctx, head, images); err != nil {
		return "", err
	}
	r.update(func(j *journal) {
		j.Before = &GoodState{Commit: head, Images: images, At: r.e.now().UTC()}
		j.Event.PreviousCommit = head
	})
	return fmt.Sprintf("%s con %d imágenes anotadas", short(head), len(images)), nil
}

// checkout deja el código en el commit que devuelve sha.
func (r *run) checkout(sha func() string) stepFunc {
	return func(ctx context.Context, tail *runner.Tail) (string, error) {
		r.update(func(j *journal) { j.Touched = true })
		if err := r.git.Checkout(ctx, r.j.Project.Branch, sha(), tail); err != nil {
			return "", deployError(proto.DeployInternal, "el checkout falló: %v", err)
		}
		return short(sha()), nil
	}
}

// up recrea los contenedores con las imágenes que hay. Antes anota en el
// diario que se empezó, por si el agent se reinicia en el medio.
func (r *run) up(ctx context.Context, tail *runner.Tail) (string, error) {
	var err error
	r.update(func(j *journal) {
		j.UpStarted = true
		err = r.e.store.saveJournal(j)
	})
	if err != nil {
		return "", err
	}
	return "", r.rt.Up(ctx, tail)
}

// declared corre un paso declarado.
func (r *run) declared(s projectspec.Step) stepFunc {
	return func(ctx context.Context, tail *runner.Tail) (string, error) {
		if err := r.rt.Run(ctx, s, tail); err != nil {
			return "", stepFailure(err, proto.DeployStepFailed, strings.Join(s.Command, " ")+" falló")
		}
		return strings.Join(s.Command, " "), nil
	}
}

// verify es la ventana de verificación (ADR-015, punto 6): los contenedores y
// el healthcheck HTTP tienen que estar bien tres veces seguidas antes de que
// se cumpla el plazo.
func (r *run) verify(ctx context.Context, tail *runner.Tail) (string, error) {
	return r.waitHealthy(ctx, tail, VerifyWindow, r.j.Project.Healthcheck)
}

func (r *run) waitHealthy(ctx context.Context, tail *runner.Tail, window time.Duration, hc *proto.HealthcheckSpec) (string, error) {
	deadline := r.e.now().Add(window)
	passes := 0
	last := ""
	for {
		ok, detail, err := r.healthy(ctx, hc)
		if err != nil {
			return "", err
		}
		if detail != last {
			tail.Add(detail)
			last = detail
		}
		if ok {
			passes++
			if passes >= verifyPasses {
				return detail, nil
			}
		} else {
			passes = 0
		}
		if !r.e.now().Before(deadline) {
			return "", deployError(proto.DeployUnhealthy, "no quedó bien en %d s: %s", int(window.Seconds()), detail)
		}
		if err := r.e.sleep(ctx, verifyInterval); err != nil {
			return "", err
		}
	}
}

// healthy mira una vez los contenedores y el healthcheck HTTP.
func (r *run) healthy(ctx context.Context, hc *proto.HealthcheckSpec) (bool, string, error) {
	status, err := r.rt.Check(ctx)
	if err != nil {
		return false, "", err
	}
	if !status.Ready {
		return false, status.Detail, nil
	}
	if hc == nil {
		return true, status.Detail, nil
	}
	result := r.e.health.Check(ctx, *hc)
	if result.State != proto.HealthPassing {
		return false, "healthcheck HTTP: " + result.Detail, nil
	}
	return true, fmt.Sprintf("%s; healthcheck HTTP %d en %d ms", status.Detail, result.StatusCode, result.LatencyMS), nil
}

// succeed anota el estado bueno nuevo y deja la foto final.
func (r *run) succeed() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.e.root), time.Minute)
	defer cancel()

	images, err := r.rt.Snapshot(ctx)
	if err != nil {
		r.e.log.Warn("no se pudieron anotar las imágenes del deploy bueno", "deploy", r.j.Event.DeployID, "error", err)
	} else {
		current := GoodState{Commit: r.j.Event.Commit, Images: images, At: r.e.now().UTC()}
		st, err := r.e.store.pushState(r.j.Project.Name, current, r.j.Before)
		if err != nil {
			r.e.log.Warn("no se pudo anotar el estado bueno", "deploy", r.j.Event.DeployID, "error", err)
		} else if err := r.rt.Prune(ctx, st.commits()); err != nil {
			r.e.log.Warn("no se pudieron quitar etiquetas de rollback viejas", "deploy", r.j.Event.DeployID, "error", err)
		}
	}

	r.update(func(j *journal) {
		j.Event.Status = proto.DeploySucceeded
		j.Event.FinishedAt = maxTime(r.e.now().UTC().Truncate(time.Millisecond), j.Event.StartedAt)
	})
	r.emit()
	r.e.log.Info("deploy terminado", "deploy", r.j.Event.DeployID, "proyecto", r.j.Event.Project, "commit", short(r.j.Event.Commit))
}

// fail vuelve atrás si hace falta y deja la foto final.
func (r *run) fail(cause error) {
	code, message := codeOf(cause)
	r.e.log.Warn("el deploy falló", "deploy", r.j.Event.DeployID, "proyecto", r.j.Event.Project, "codigo", code, "detalle", message)

	status := proto.DeployFailed
	if r.j.Touched {
		if err := r.restore(message); err != nil {
			code, message = proto.DeployRollbackFailed, fmt.Sprintf("%s; y no pudo volver atrás: %v", message, err)
		} else {
			status = proto.DeployRolledBack
		}
	}
	r.finish(status, code, message)
}

// interrupted cierra un deploy que quedó a medias porque el agent se
// reinició: vuelve atrás si ya había tocado algo.
func (r *run) interrupted() {
	now := r.e.now().UTC().Truncate(time.Millisecond)
	r.update(func(j *journal) {
		// El paso que estaba corriendo murió con el agent.
		for i := range j.Event.Steps {
			if j.Event.Steps[i].Status == proto.StepRunning {
				j.Event.Steps[i].Status = proto.StepFailed
				j.Event.Steps[i].FinishedAt = maxTime(now, j.Event.Steps[i].StartedAt)
				j.Event.Steps[i].Detail = "el agent se reinició en medio de este paso"
			}
		}
	})
	message := "el agent se reinició en medio del deploy"
	code, status := proto.DeployInterrupted, proto.DeployFailed
	if r.j.Touched {
		var err error
		if r.git, err = r.e.tools.Git(r.j.Project, r.e.keyFor(r.j.Project.Name)); err == nil {
			r.rt, err = r.e.runtimeFor(r.j.Project)
		}
		if err == nil {
			err = r.restore(message)
		}
		if err != nil {
			code, message = proto.DeployRollbackFailed, fmt.Sprintf("%s; y no pudo volver atrás: %v", message, err)
		} else {
			status = proto.DeployRolledBack
		}
	}
	r.finish(status, code, message)
}

// restore vuelve el proyecto a como estaba antes (la foto del diario):
// imágenes, código y, si ya se habían recreado, los contenedores.
func (r *run) restore(reason string) error {
	before := r.j.Before
	if before == nil {
		return errors.New("no hay foto de cómo estaba el proyecto")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.e.root), rollbackTimeout)
	defer cancel()

	return r.step(ctx, proto.StepRollback, "", func(ctx context.Context, tail *runner.Tail) (string, error) {
		tail.Add("volviendo a " + short(before.Commit) + ": " + reason)
		if err := r.rt.Restore(ctx, before.Images); err != nil {
			return "", err
		}
		if err := r.git.Checkout(ctx, r.j.Project.Branch, before.Commit, tail); err != nil {
			return "", fmt.Errorf("volver el código: %w", err)
		}
		if !r.j.UpStarted {
			return "el código y las imágenes volvieron; los contenedores no se habían tocado", nil
		}
		if err := r.rt.Up(ctx, tail); err != nil {
			return "", fmt.Errorf("levantar lo anterior: %w", err)
		}
		// Una verificación corta, solo de los contenedores: si lo anterior
		// tampoco levanta, hay que mirarlo.
		detail, err := r.waitHealthy(ctx, tail, rollbackVerifyWindow, nil)
		if err != nil {
			return "", fmt.Errorf("lo anterior tampoco quedó bien: %w", err)
		}
		return "volvió a " + short(before.Commit) + ": " + detail, nil
	})
}

func (r *run) finish(status proto.DeployStatus, code proto.DeployErrorCode, message string) {
	r.update(func(j *journal) {
		j.Event.Status = status
		j.Event.Code = code
		j.Event.Message = runner.Sanitize(message, proto.MaxDeployMessageLen)
		j.Event.FinishedAt = maxTime(r.e.now().UTC().Truncate(time.Millisecond), j.Event.StartedAt)
	})
	r.emit()
}

// stepFunc es lo que hace un paso. Devuelve el detalle para el panel.
type stepFunc func(ctx context.Context, tail *runner.Tail) (string, error)

// step corre un paso: lo anota en curso, manda fotos con su salida mientras
// corre, y lo cierra bien o mal.
func (r *run) step(ctx context.Context, name proto.StepName, service string, fn stepFunc) error {
	var idx int
	r.update(func(j *journal) {
		idx = len(j.Event.Steps)
		j.Event.Steps = append(j.Event.Steps, proto.DeployStep{
			Name: name, Service: service, Status: proto.StepRunning, StartedAt: r.e.now().UTC().Truncate(time.Millisecond),
		})
	})
	r.emit()

	tail := runner.NewTail(proto.MaxStepOutputLines, proto.MaxStepOutputLineLen)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.progress(idx, tail, stop)
	}()
	detail, err := fn(ctx, tail)
	close(stop)
	<-done

	r.update(func(j *journal) {
		s := &j.Event.Steps[idx]
		s.Output = tail.Lines()
		s.FinishedAt = maxTime(r.e.now().UTC().Truncate(time.Millisecond), s.StartedAt)
		if err != nil {
			s.Status = proto.StepFailed
			_, message := codeOf(err)
			s.Detail = runner.Sanitize(message, proto.MaxDeployMessageLen)
		} else {
			s.Status = proto.StepOK
			s.Detail = runner.Sanitize(detail, proto.MaxDeployMessageLen)
		}
	})
	r.emit()
	return err
}

// progress manda, cada tanto, la salida nueva del paso en curso.
func (r *run) progress(idx int, tail *runner.Tail, stop <-chan struct{}) {
	ticker := time.NewTicker(progressEvery)
	defer ticker.Stop()
	var last string
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			lines := tail.Lines()
			joined := strings.Join(lines, "\n")
			if len(lines) == 0 || joined == last {
				continue
			}
			last = joined
			r.update(func(j *journal) { j.Event.Steps[idx].Output = lines })
			r.emit()
		}
	}
}

// skip anota un paso que no hizo falta.
func (r *run) skip(name proto.StepName, detail string) {
	now := r.e.now().UTC().Truncate(time.Millisecond)
	r.update(func(j *journal) {
		j.Event.Steps = append(j.Event.Steps, proto.DeployStep{Name: name, Status: proto.StepSkipped, StartedAt: now, FinishedAt: now, Detail: detail})
	})
	r.emit()
}

// emit sube la secuencia, guarda el diario y deja la foto en la bandeja.
func (r *run) emit() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.j.Event.Seq++
	// Solo cambian los pasos: el resto de la foto lo lee la goroutine del deploy.
	r.j.Event.Steps = fit(r.j.Event).Steps
	ev := r.j.Event
	ev.Steps = cloneSteps(ev.Steps)
	if err := ev.Validate(); err != nil {
		// No pasa con lo que arma el motor; si pasara, se registra y la foto
		// no sale, en vez de mandar algo que el control plane va a rechazar.
		r.e.log.Error("foto de deploy inválida", "deploy", ev.DeployID, "error", err)
		return
	}
	if err := r.e.store.saveJournal(r.j); err != nil {
		r.e.log.Warn("no se pudo guardar el diario del deploy", "deploy", ev.DeployID, "error", err)
	}
	r.e.out.Put(ev)
}

func cloneSteps(steps []proto.DeployStep) []proto.DeployStep {
	out := make([]proto.DeployStep, len(steps))
	for i, s := range steps {
		s.Output = append([]string(nil), s.Output...)
		out[i] = s
	}
	return out
}

// stepFailure traduce el error de un programa al código del deploy.
func stepFailure(err error, code proto.DeployErrorCode, what string) error {
	var exit *runner.ExitError
	switch {
	case errors.Is(err, runner.ErrTimeout):
		return deployError(proto.DeployTimeout, "%s: se pasó de su tiempo", what)
	case errors.As(err, &exit):
		return deployError(code, "%s (salió con código %d)", what, exit.Code)
	default:
		return deployError(code, "%s: %v", what, err)
	}
}

func lastLine(tail *runner.Tail, err error) string {
	if lines := tail.Lines(); len(lines) > 0 {
		return lines[len(lines)-1]
	}
	return err.Error()
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func maxTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}
