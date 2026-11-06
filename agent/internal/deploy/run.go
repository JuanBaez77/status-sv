package deploy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/gitops"
	"github.com/JuanBaez77/status-sv/agent/internal/runner"
	"github.com/JuanBaez77/status-sv/internal/projectspec"
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
	// unit es la unidad a reiniciar en un reinicio; vacía es todas.
	unit string

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
	switch r.j.Event.Kind {
	case proto.DeployKindRollback:
		err = r.rollbackSteps(ctx)
	case proto.DeployKindBootstrap:
		err = r.bootstrapSteps(ctx)
	case proto.DeployKindRestart:
		err = r.restartSteps(ctx)
	default:
		err = r.deploySteps(ctx)
	}
	if err == nil && r.j.Event.Kind == proto.DeployKindRestart {
		// Un reinicio no deja un estado bueno nuevo: el código y las
		// imágenes son los mismos.
		r.finish(proto.DeploySucceeded, "", "")
		r.e.log.Info("reinicio terminado", "deploy", r.j.Event.DeployID, "proyecto", r.j.Event.Project)
		return
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
	return r.launch(ctx)
}

// bootstrapSteps son los pasos de un primer deploy (ADR-021): los de un
// deploy, sin la foto de lo anterior, porque no hay nada anterior. El
// repositorio se inicia acá, en un workdir que una persona dejó preparado.
func (r *run) bootstrapSteps(ctx context.Context) error {
	if err := r.step(ctx, proto.StepPreflight, "", r.firstPreflight); err != nil {
		return err
	}
	if err := r.step(ctx, proto.StepFetch, "", r.clone); err != nil {
		return err
	}
	if err := r.step(ctx, proto.StepCheckout, "", r.firstCheckout); err != nil {
		return err
	}
	return r.launch(ctx)
}

// launch prepara el código que quedó en el workdir, lo pone a correr y lo
// verifica: lo que tienen en común un deploy y un primer deploy.
func (r *run) launch(ctx context.Context) error {
	if err := r.builds(ctx); err != nil {
		return err
	}
	for i, s := range r.spec.Pre {
		if err := r.labeled(ctx, proto.StepPre, s.Service, commandLine(s), r.declared(phasePre, i, s)); err != nil {
			return err
		}
	}
	if err := r.labeled(ctx, r.rt.StartStep(), "", r.rt.StartLabel(), r.up); err != nil {
		return err
	}
	if err := r.step(ctx, proto.StepHealthcheck, "", r.verify); err != nil {
		return err
	}
	for i, s := range r.spec.Post {
		if err := r.labeled(ctx, proto.StepPost, s.Service, commandLine(s), r.declared(phasePost, i, s)); err != nil {
			return err
		}
	}
	return nil
}

// builds corre lo que prepara el código nuevo: con Docker es un solo paso, y
// con systemd, uno por cada paso declarado en deploy.build.
func (r *run) builds(ctx context.Context) error {
	for _, b := range r.rt.Builds() {
		err := r.labeled(ctx, proto.StepBuild, "", b.label, func(ctx context.Context, tail *runner.Tail) (string, error) {
			return b.label, b.run(ctx, tail)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// restartSteps son los pasos de un reinicio (ADR-026): revisar, reiniciar y
// verificar. No toca el código ni tiene vuelta atrás: si no queda bien, el
// reinicio termina en failed y lo que corre queda como quedó.
func (r *run) restartSteps(ctx context.Context) error {
	if err := r.step(ctx, proto.StepPreflight, "", r.restartPreflight); err != nil {
		return err
	}
	label := r.unit
	if label == "" {
		label = r.rt.StartLabel()
	}
	if err := r.labeled(ctx, proto.StepRestart, "", label, func(ctx context.Context, tail *runner.Tail) (string, error) {
		return label, r.rt.Restart(ctx, r.unit, tail)
	}); err != nil {
		return err
	}
	return r.step(ctx, proto.StepHealthcheck, "", r.verify)
}

// restartPreflight revisa lo propio del runtime y que la unidad pedida sea
// del proyecto, y anota el commit que corre, para que la historia no cambie
// de versión por un reinicio.
func (r *run) restartPreflight(ctx context.Context, _ *runner.Tail) (string, error) {
	var err error
	if r.rt, err = r.e.runtimeFor(r.j, &r.spec); err != nil {
		return "", deployError(proto.DeployPreflightFailed, "%v", err)
	}
	detail, err := r.rt.Preflight(ctx)
	if err != nil {
		return "", err
	}
	units, err := r.rt.Units(ctx)
	if err != nil {
		return "", deployError(proto.DeployPreflightFailed, "%v", err)
	}
	if len(units) == 0 {
		return "", deployError(proto.DeployPreflightFailed, "no hay nada del proyecto corriendo para reiniciar")
	}
	if r.unit != "" && !slices.Contains(units, r.unit) {
		return "", deployError(proto.DeployPreflightFailed, "el proyecto no tiene la unidad %s; las suyas son %s", r.unit, strings.Join(units, ", "))
	}
	if st, err := r.e.store.loadState(r.j.Project.Name); err == nil && len(st.History) > 0 {
		r.update(func(j *journal) { j.Event.Commit = st.History[0].Commit })
	}
	return detail, nil
}

// rollbackSteps son los pasos de un rollback manual: a donde estaba el
// proyecto antes del último deploy bueno, sin pasos declarados. Con Docker no
// compila, porque vuelve a las imágenes guardadas; con systemd corre de nuevo
// lo que prepara el código (ADR-019).
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
	if r.rt.RebuildsOnRollback() {
		if err := r.builds(ctx); err != nil {
			return err
		}
	} else {
		r.skip(proto.StepBuild, "un rollback no compila: usa las imágenes guardadas")
	}
	if err := r.labeled(ctx, r.rt.StartStep(), "", r.rt.StartLabel(), func(ctx context.Context, tail *runner.Tail) (string, error) {
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
	if r.rt, err = r.e.runtimeFor(r.j, &r.spec); err != nil {
		return "", deployError(proto.DeployPreflightFailed, "%v", err)
	}
	switch _, err := r.git.Head(ctx); {
	case errors.Is(err, gitops.ErrNotRepository), errors.Is(err, gitops.ErrNoCommit):
		return "", deployError(proto.DeployPreflightFailed,
			"%s no tiene el código clonado (o el agent no lo puede leer); si el proyecto no se desplegó nunca, lo que va es un primer deploy", p.Workdir)
	case err != nil:
		return "", deployError(proto.DeployPreflightFailed, "no se pudo leer el repositorio: %v", err)
	}
	if err := r.clean(ctx); err != nil {
		return "", err
	}
	return r.rt.Preflight(ctx)
}

// clean revisa que nadie haya cambiado a mano archivos versionados.
func (r *run) clean(ctx context.Context) error {
	err := r.git.CheckClean(ctx)
	switch {
	case errors.Is(err, gitops.ErrDirty):
		return deployError(proto.DeployDirtyWorktree, "hay archivos versionados cambiados a mano en %s; el deploy no los pisa", r.j.Project.Workdir)
	case err != nil:
		return deployError(proto.DeployPreflightFailed, "%v", err)
	}
	return nil
}

// firstPreflight es el chequeo previo de un primer deploy (ADR-021): el
// proyecto no se desplegó nunca, nada suyo corre, y su workdir existe y
// todavía no tiene el código (o tiene el clon de un intento anterior).
func (r *run) firstPreflight(ctx context.Context, _ *runner.Tail) (string, error) {
	p := r.j.Project
	var err error
	if r.git, err = r.e.tools.Git(p, r.e.keyFor(p.Name)); err != nil {
		return "", deployError(proto.DeployPreflightFailed, "%v", err)
	}
	if r.rt, err = r.e.runtimeFor(r.j, &r.spec); err != nil {
		return "", deployError(proto.DeployPreflightFailed, "%v", err)
	}

	st, err := r.e.store.loadState(p.Name)
	if err != nil {
		return "", err
	}
	if len(st.History) > 0 {
		return "", deployError(proto.DeployAlreadyDeployed, "el proyecto ya se desplegó (está en %s): lo que sigue son deploys", short(st.History[0].Commit))
	}
	running, err := r.rt.Running(ctx)
	if err != nil {
		return "", deployError(proto.DeployPreflightFailed, "%v", err)
	}
	if running != "" {
		return "", deployError(proto.DeployAlreadyDeployed, "el proyecto ya tiene algo corriendo (%s): lo que sigue son deploys", running)
	}

	cloned, err := r.git.Cloned()
	switch {
	case errors.Is(err, gitops.ErrNoWorkdir):
		return "", deployError(proto.DeployPreflightFailed, "el workdir %s no existe: lo crea una persona, con permiso de escritura para el agent", p.Workdir)
	case err != nil:
		return "", deployError(proto.DeployPreflightFailed, "%v", err)
	}
	state := "workdir sin clonar"
	if cloned {
		// Lo que dejó un intento anterior, que no llegó a quedar bien.
		state = "workdir con el clon de un intento anterior"
		switch _, err := r.git.Head(ctx); {
		case errors.Is(err, gitops.ErrNoCommit):
		case err != nil:
			return "", deployError(proto.DeployPreflightFailed, "no se pudo leer el repositorio de %s: %v", p.Workdir, err)
		default:
			if err := r.clean(ctx); err != nil {
				return "", err
			}
		}
	}

	detail, err := r.rt.Available(ctx)
	if err != nil {
		return "", err
	}
	return state + "; " + detail, nil
}

// clone inicia el repositorio en el workdir, si no estaba, y trae la rama
// declarada. No configura ningún origin: como en un deploy, el código sale de
// la URL que se arma desde el repo declarado.
func (r *run) clone(ctx context.Context, tail *runner.Tail) (string, error) {
	if err := r.git.Init(ctx, tail); err != nil {
		return "", stepFailure(err, proto.DeployFetchFailed, "no se pudo iniciar el repositorio en "+r.j.Project.Workdir)
	}
	return r.fetch(ctx, tail)
}

// firstCheckout deja el código en el workdir y, ya con el código, revisa lo
// que el chequeo previo no podía: el archivo de Compose, por ejemplo. Git no
// pisa un archivo que ya estaba (un .env que dejó una persona): si choca con
// uno versionado, falla.
func (r *run) firstCheckout(ctx context.Context, tail *runner.Tail) (string, error) {
	detail, err := r.checkout(func() string { return r.j.Event.Commit })(ctx, tail)
	if err != nil {
		return "", err
	}
	if _, err := r.rt.Preflight(ctx); err != nil {
		return "", err
	}
	return detail, nil
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
	if noted := r.rt.Describe(images); noted != "" {
		return short(head) + " con " + noted, nil
	}
	return short(head), nil
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

// up pone a correr lo que quedó preparado: recrea los contenedores o reinicia
// las unidades. Antes anota en el diario que se empezó, por si el agent se
// reinicia en el medio.
func (r *run) up(ctx context.Context, tail *runner.Tail) (string, error) {
	var err error
	r.update(func(j *journal) {
		j.UpStarted = true
		err = r.e.store.saveJournal(j)
	})
	if err != nil {
		return "", err
	}
	return r.rt.StartLabel(), r.rt.Up(ctx, tail)
}

// Las listas de pasos declarados del project.yaml.
const (
	phaseBuild = "build"
	phasePre   = "pre"
	phasePost  = "post"
)

// commandLine es un paso declarado como se muestra: su comando en un renglón.
func commandLine(s projectspec.Step) string {
	return strings.Join(s.Command, " ")
}

// declared corre un paso declarado: el número index de la lista phase.
func (r *run) declared(phase string, index int, s projectspec.Step) stepFunc {
	return func(ctx context.Context, tail *runner.Tail) (string, error) {
		if err := r.rt.Run(ctx, phase, index, s, tail); err != nil {
			return "", stepFailure(err, proto.DeployStepFailed, commandLine(s)+" falló")
		}
		return commandLine(s), nil
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

	r.enableAtBoot(ctx)

	r.update(func(j *journal) {
		j.Event.Status = proto.DeploySucceeded
		j.Event.FinishedAt = maxTime(r.e.now().UTC().Truncate(time.Millisecond), j.Event.StartedAt)
	})
	r.emit()
	r.e.log.Info("deploy terminado", "deploy", r.j.Event.DeployID, "proyecto", r.j.Event.Project, "commit", short(r.j.Event.Commit))
}

// enableAtBoot habilita para el arranque lo que un primer deploy dejó andando
// (ADR-022): hasta el Sprint 6 lo hacía una persona a mano. Que falle no
// deshace el deploy, que quedó bien: se anota en la salida del paso que
// arrancó el proyecto, con cómo hacerlo a mano.
func (r *run) enableAtBoot(ctx context.Context) {
	if r.j.Event.Kind != proto.DeployKindBootstrap {
		return
	}
	enabler, ok := r.rt.(interface {
		EnableAtBoot(ctx context.Context) (string, error)
	})
	if !ok {
		return
	}
	line, err := enabler.EnableAtBoot(ctx)
	if err != nil {
		r.e.log.Warn("no se pudieron habilitar las unidades del primer deploy", "deploy", r.j.Event.DeployID, "error", err)
		line = "no se pudieron habilitar las unidades para el arranque (" + err.Error() + "): habilitalas con systemctl enable"
	}
	r.update(func(j *journal) {
		for i := len(j.Event.Steps) - 1; i >= 0; i-- {
			if j.Event.Steps[i].Name == r.rt.StartStep() {
				out := append(j.Event.Steps[i].Output, clip(line))
				if len(out) > proto.MaxStepOutputLines {
					out = out[len(out)-proto.MaxStepOutputLines:]
				}
				j.Event.Steps[i].Output = out
				return
			}
		}
	})
}

// clip acota un renglón de la salida de un paso.
func clip(line string) string {
	if len(line) > proto.MaxStepOutputLineLen {
		return strings.ToValidUTF8(line[:proto.MaxStepOutputLineLen], "")
	}
	return line
}

// fail vuelve atrás si hace falta y deja la foto final.
func (r *run) fail(cause error) {
	code, message := codeOf(cause)
	r.e.log.Warn("el deploy falló", "deploy", r.j.Event.DeployID, "proyecto", r.j.Event.Project, "codigo", code, "detalle", message)

	status := proto.DeployFailed
	switch undone, err := r.undo(message); {
	case err != nil:
		code, message = proto.DeployRollbackFailed, r.undoFailed(message, err)
	case undone:
		status = proto.DeployRolledBack
	}
	r.finish(status, code, message)
}

// undo deshace lo que el deploy llegó a tocar, y dice si había algo que
// deshacer. Un deploy vuelve a como estaba el proyecto. Un primer deploy no
// tiene a dónde volver: baja lo que levantó, si llegó a levantar algo, y el
// código clonado queda (ADR-021).
func (r *run) undo(reason string) (bool, error) {
	if r.j.Event.Kind == proto.DeployKindRestart {
		// Un reinicio no tiene a dónde volver.
		return false, nil
	}
	if r.j.Event.Kind == proto.DeployKindBootstrap {
		if !r.j.UpStarted {
			return false, nil
		}
		return true, r.teardown(reason)
	}
	if !r.j.Touched {
		return false, nil
	}
	return true, r.restore(reason)
}

func (r *run) undoFailed(message string, err error) string {
	what := "volver atrás"
	if r.j.Event.Kind == proto.DeployKindBootstrap {
		what = "bajar lo que había levantado"
	}
	return fmt.Sprintf("%s; y no pudo %s: %v", message, what, err)
}

// teardown baja lo que levantó un primer deploy que no quedó bien.
func (r *run) teardown(reason string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.e.root), rollbackTimeout)
	defer cancel()

	return r.step(ctx, proto.StepRollback, "", func(ctx context.Context, tail *runner.Tail) (string, error) {
		tail.Add("bajando lo que levantó este primer deploy: " + reason)
		if err := r.rt.Down(ctx, tail); err != nil {
			return "", err
		}
		return "bajó lo que había levantado; el código quedó en el workdir", nil
	})
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
			r.rt, err = r.e.runtimeFor(r.j, nil)
		}
		undone := false
		if err == nil {
			undone, err = r.undo(message)
		}
		switch {
		case err != nil:
			code, message = proto.DeployRollbackFailed, r.undoFailed(message, err)
		case undone:
			status = proto.DeployRolledBack
		}
	}
	r.finish(status, code, message)
}

// restore vuelve el proyecto a como estaba antes (la foto del diario): lo que
// se había guardado, el código y, si ya se había tocado, lo que corre. Con
// systemd, además, prepara de nuevo el código anterior (ADR-019).
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
		if r.rt.RebuildsOnRollback() {
			for _, b := range r.rt.Builds() {
				tail.Add("preparando lo anterior: " + b.label)
				if err := b.run(ctx, tail); err != nil {
					return "", fmt.Errorf("preparar lo anterior: %w", err)
				}
			}
		}
		if !r.j.UpStarted {
			return r.rt.Untouched(), nil
		}
		if err := r.rt.Up(ctx, tail); err != nil {
			return "", fmt.Errorf("levantar lo anterior: %w", err)
		}
		// Una verificación corta, solo de lo que corre, sin el healthcheck
		// HTTP: si lo anterior tampoco levanta, hay que mirarlo.
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
	return r.labeled(ctx, name, service, "", fn)
}

// labeled es step con un detalle desde el arranque: el comando de un paso
// declarado se ve mientras corre, no recién cuando termina.
func (r *run) labeled(ctx context.Context, name proto.StepName, service, label string, fn stepFunc) error {
	var idx int
	r.update(func(j *journal) {
		idx = len(j.Event.Steps)
		j.Event.Steps = append(j.Event.Steps, proto.DeployStep{
			Name: name, Service: service, Status: proto.StepRunning, StartedAt: r.e.now().UTC().Truncate(time.Millisecond),
			Detail: runner.Sanitize(label, proto.MaxDeployMessageLen),
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
