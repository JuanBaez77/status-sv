// Package projects lee los proyectos declarados en el servidor, prueba su
// healthcheck y decide cuándo mandarlos al control plane (ADR-012).
//
// El directorio es de root y el agent solo lo lee. Se relee entero cada 30 s:
// son unos pocos archivos chicos, y releerlos es más simple y más seguro que
// seguir fechas de modificación. La foto sale solo si cambió algo, o cada 5
// minutos para que el control plane sepa que sigue valiendo.
package projects

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/JuanBaez77/status-sv/agent/internal/outbox"
	"github.com/JuanBaez77/status-sv/internal/projectspec"
	"github.com/JuanBaez77/status-sv/proto"
)

// DefaultDir es donde una persona declara los proyectos del servidor.
const DefaultDir = "/etc/status-sv/projects.d"

// Cuándo se relee el directorio y cuándo sale una foto sin cambios.
const (
	DefaultRefresh = 30 * time.Second
	DefaultResend  = 5 * time.Minute
)

// Outbox es la bandeja de salida de los proyectos.
type Outbox = outbox.Box[proto.ProjectsState]

// NewOutbox arma una bandeja vacía.
func NewOutbox() *Outbox {
	return outbox.New(func(s proto.ProjectsState) time.Time { return s.TakenAt })
}

// Read lee el directorio y arma la foto. Nunca falla: si el directorio no
// existe, la fuente queda como no disponible (un servidor sin proyectos); si no
// se puede leer, como error, y el control plane conserva lo anterior.
func Read(dir string, now time.Time) proto.ProjectsState {
	state := proto.ProjectsState{TakenAt: now.UTC(), Source: proto.RuntimeSource{Status: proto.SourceOK}}

	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		state.Source = proto.RuntimeSource{Status: proto.SourceUnavailable, Detail: "no existe " + dir}
		return state
	case err != nil:
		state.Source = proto.SourceFailed(err)
		return state
	}

	// seen es el archivo de cada proyecto aceptado, para marcar los dos si
	// aparece otro con el mismo nombre.
	seen := make(map[string]string)
	for _, entry := range entries {
		filename := entry.Name()
		name, err := projectspec.NameFromFile(filename)
		// Lo que no es un .yaml (un README, un respaldo) y los archivos ocultos
		// se ignoran sin avisar.
		if err != nil || strings.HasPrefix(filename, ".") {
			continue
		}
		if len(state.Projects) >= proto.MaxProjects || len(state.Invalid) >= proto.MaxInvalidProjects {
			addInvalid(&state, filename, fmt.Errorf("hay más de %d archivos; este queda afuera", proto.MaxProjects))
			continue
		}

		project, err := readOne(filepath.Join(dir, filename), entry)
		switch {
		case err != nil:
			addInvalid(&state, filename, err)
		case seen[name] != "":
			// a.yaml y a.yml declaran el mismo proyecto: ninguno gana por
			// orden alfabético, los dos se marcan.
			duplicate := errors.New("hay otro archivo con el mismo nombre de proyecto")
			if before := len(state.Projects); before > 0 {
				state.Projects = slices.DeleteFunc(state.Projects, func(p proto.Project) bool { return p.Name == name })
				if len(state.Projects) < before {
					addInvalid(&state, seen[name], duplicate)
				}
			}
			addInvalid(&state, filename, duplicate)
		default:
			seen[name] = filename
			state.Projects = append(state.Projects, project)
		}
	}
	return state
}

// ErrNotDeclared es que no hay un archivo de proyecto con ese nombre.
var ErrNotDeclared = errors.New("el proyecto no está declarado en este servidor")

// ReadSpec lee la declaración de un proyecto por su nombre, con sus pasos de
// deploy. Es lo que usa el motor de deploy al arrancar cada deploy: se
// despliega con lo que dice el archivo en ese momento. Aplica las mismas
// reglas que Read: solo archivos regulares, y si están name.yaml y name.yml a
// la vez, ninguno vale.
func ReadSpec(dir, name string) (projectspec.Spec, error) {
	if !proto.ValidProjectName(name) {
		return projectspec.Spec{}, ErrNotDeclared
	}
	var found []string
	for _, ext := range []string{".yaml", ".yml"} {
		_, err := os.Lstat(filepath.Join(dir, name+ext))
		switch {
		case err == nil:
			found = append(found, name+ext)
		case !errors.Is(err, fs.ErrNotExist):
			// Un permiso que falta no es lo mismo que un proyecto que no
			// existe: que el error lo diga.
			return projectspec.Spec{}, fmt.Errorf("no se puede leer %s: %w", dir, err)
		}
	}
	switch len(found) {
	case 0:
		return projectspec.Spec{}, ErrNotDeclared
	case 2:
		return projectspec.Spec{}, errors.New("hay otro archivo con el mismo nombre de proyecto")
	}
	path := filepath.Join(dir, found[0])
	info, err := os.Lstat(path)
	if err != nil {
		return projectspec.Spec{}, ErrNotDeclared
	}
	data, err := readRegular(path, fs.FileInfoToDirEntry(info))
	if err != nil {
		return projectspec.Spec{}, err
	}
	return projectspec.ParseSpec(found[0], data)
}

// readOne lee un archivo de proyecto. Solo acepta archivos regulares: un
// enlace simbólico podría apuntar a cualquier archivo que el agent pueda leer.
func readOne(path string, entry fs.DirEntry) (proto.Project, error) {
	data, err := readRegular(path, entry)
	if err != nil {
		return proto.Project{}, err
	}
	project, err := projectspec.Parse(filepath.Base(path), data)
	if err != nil {
		return proto.Project{}, err
	}
	// El hash del archivo dice si un proyecto propuesto se declaró tal cual
	// (ADR-022): es el mismo que el de la propuesta si nadie lo cambió.
	project.SpecHash = proto.SpecHash(data)
	return project, nil
}

// readRegular lee un archivo regular del directorio de proyectos, con el tope
// de tamaño del parser.
func readRegular(path string, entry fs.DirEntry) ([]byte, error) {
	if !entry.Type().IsRegular() {
		return nil, errors.New("no es un archivo regular (los enlaces simbólicos no se siguen)")
	}
	f, err := os.Open(path) //nolint:gosec // la ruta sale del directorio de proyectos, que es de root
	if err != nil {
		return nil, errors.New("no se puede abrir")
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, projectspec.MaxFileBytes+1))
	if err != nil {
		return nil, errors.New("no se puede leer")
	}
	return data, nil
}

// addInvalid anota un archivo inválido, con el nombre y el error recortados
// para que entren en el contrato.
func addInvalid(state *proto.ProjectsState, filename string, err error) {
	if len(state.Invalid) >= proto.MaxInvalidProjects {
		return
	}
	state.Invalid = append(state.Invalid, proto.InvalidProject{
		File:  clean(filename, proto.MaxProjectFileLen),
		Error: clean(err.Error(), proto.MaxProjectErrorLen),
	})
}

// clean deja un texto imprimible y dentro de max bytes, sin cortar un carácter.
func clean(s string, maxLen int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' {
			return '?'
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	if len(s) > maxLen {
		s = strings.ToValidUTF8(s[:maxLen], "")
	}
	if s == "" {
		return "?"
	}
	return s
}

// Runner relee el directorio, prueba los healthchecks declarados y deja en la
// bandeja las fotos que cambiaron.
type Runner struct {
	dir     string
	outbox  *Outbox
	checker HealthChecker
	log     *slog.Logger
	now     func() time.Time

	refresh, resend, healthEvery time.Duration

	// proposals lista las propuestas de proyectos nuevos que esperan una
	// persona (ADR-022). Es opcional. wake adelanta la próxima lectura cuando
	// llega o se retira una.
	proposals func() ([]proto.PendingProposal, error)
	wake      chan struct{}

	mu sync.Mutex
	// read es la última lectura del directorio, sin healthchecks; current es
	// la última foto armada, con el resultado de cada uno.
	read     proto.ProjectsState
	current  proto.ProjectsState
	health   map[string]healthEntry
	lastSig  [sha256.Size]byte
	lastSent time.Time
}

// healthEntry es el último healthcheck de un proyecto y lo que se probó: si
// alguien cambia la URL, el resultado viejo ya no vale.
type healthEntry struct {
	spec   proto.HealthcheckSpec
	result proto.HealthResult
}

// NewRunner arma el Runner sobre un directorio. Con checker nil no se prueban
// los healthchecks.
func NewRunner(dir string, o *Outbox, checker HealthChecker, log *slog.Logger) *Runner {
	return &Runner{
		dir:         dir,
		outbox:      o,
		checker:     checker,
		log:         log,
		now:         time.Now,
		refresh:     DefaultRefresh,
		resend:      DefaultResend,
		healthEvery: DefaultHealthInterval,
		health:      make(map[string]healthEntry),
		wake:        make(chan struct{}, 1),
	}
}

// SetProposals le da al Runner de dónde sacar las propuestas guardadas, que
// viajan en la misma foto. Se llama antes de Run.
func (r *Runner) SetProposals(list func() ([]proto.PendingProposal, error)) {
	r.proposals = list
}

// Refresh pide una lectura ya, sin esperar el próximo refresco: llegó o se
// retiró una propuesta, y el control plane tiene que saberlo.
func (r *Runner) Refresh() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run relee el directorio hasta que se cancela ctx: una vez al arrancar y
// después en cada refresco. Los healthchecks corren aparte, porque una prueba
// puede tardar hasta 30 s y no tiene que demorar la lectura.
func (r *Runner) Run(ctx context.Context) {
	r.take(ctx)

	var wg sync.WaitGroup
	defer wg.Wait()
	if r.checker != nil {
		wg.Go(func() { r.runHealth(ctx) })
	}

	ticker := time.NewTicker(r.refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.take(ctx)
		case <-r.wake:
			r.take(ctx)
		}
	}
}

// Current devuelve los proyectos de la última foto.
func (r *Runner) Current() []proto.Project {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.current.Projects)
}

func (r *Runner) take(ctx context.Context) {
	state := Read(r.dir, r.now())
	if r.proposals != nil {
		pending, err := r.proposals()
		if err != nil {
			r.log.WarnContext(ctx, "no se pudieron leer las propuestas de proyectos", "error", err)
		}
		state.Proposals = pending
	}
	r.mu.Lock()
	r.read = state
	r.mu.Unlock()
	r.publish(ctx)
}

// publish arma la foto con la última lectura y los últimos healthchecks, y la
// deja en la bandeja si cambió algo o si pasó el tiempo de reenvío.
func (r *Runner) publish(ctx context.Context) {
	r.mu.Lock()
	state := r.withHealth(r.read)
	sig := signature(state)
	r.current = state
	unchanged := sig == r.lastSig && r.now().Sub(r.lastSent) < r.resend
	if !unchanged {
		r.lastSig, r.lastSent = sig, r.now()
		// Adentro del candado: la lectura y los healthchecks publican desde
		// dos goroutines, y una foto vieja no puede quedar después de una nueva.
		r.outbox.Put(state)
	}
	r.mu.Unlock()
	if unchanged {
		return
	}

	r.log.InfoContext(ctx, "proyectos declarados leídos",
		"directorio", r.dir,
		"fuente", state.Source.Status,
		"validos", len(state.Projects),
		"invalidos", len(state.Invalid),
		"propuestas", len(state.Proposals),
	)
	for _, invalid := range state.Invalid {
		r.log.WarnContext(ctx, "archivo de proyecto inválido", "archivo", invalid.File, "error", invalid.Error)
	}
}

// withHealth copia la lectura y le suma a cada proyecto su último healthcheck,
// si se probó con lo que declara hoy. Se llama con r.mu tomado.
func (r *Runner) withHealth(read proto.ProjectsState) proto.ProjectsState {
	state := read
	state.Projects = slices.Clone(read.Projects)
	for i, p := range state.Projects {
		entry, ok := r.health[p.Name]
		if p.Healthcheck == nil || !ok || entry.spec != *p.Healthcheck {
			continue
		}
		result := entry.result
		state.Projects[i].Health = &result
	}
	return state
}

// runHealth prueba los healthchecks al arrancar y después cada minuto.
func (r *Runner) runHealth(ctx context.Context) {
	r.checkHealth(ctx)
	ticker := time.NewTicker(r.healthEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.checkHealth(ctx)
		}
	}
}

// checkHealth prueba en paralelo el healthcheck de cada proyecto de la última
// lectura y publica. Con 50 proyectos como máximo y 30 s de timeout, una
// vuelta nunca se superpone con la siguiente.
func (r *Runner) checkHealth(ctx context.Context) {
	r.mu.Lock()
	projects := slices.Clone(r.read.Projects)
	r.mu.Unlock()

	results := make([]proto.HealthResult, len(projects))
	var wg sync.WaitGroup
	for i, p := range projects {
		if p.Healthcheck == nil {
			continue
		}
		spec := *p.Healthcheck
		wg.Go(func() { results[i] = r.checker.Check(ctx, spec) })
	}
	wg.Wait()
	// Al apagar, las pruebas cortadas no son una caída del proyecto.
	if ctx.Err() != nil {
		return
	}

	r.mu.Lock()
	next := make(map[string]healthEntry, len(projects))
	for i, p := range projects {
		if p.Healthcheck == nil {
			continue
		}
		entry := healthEntry{spec: *p.Healthcheck, result: results[i]}
		before, had := r.health[p.Name]
		next[p.Name] = entry
		if had && before.spec == entry.spec && before.result.State == entry.result.State {
			continue
		}
		r.logHealth(ctx, p.Name, entry.result)
	}
	r.health = next
	r.mu.Unlock()

	r.publish(ctx)
}

// logHealth anota el primer resultado de un proyecto y cada cambio de estado.
func (r *Runner) logHealth(ctx context.Context, name string, h proto.HealthResult) {
	attrs := []any{"proyecto", name, "codigo", h.StatusCode, "latencia_ms", h.LatencyMS}
	if h.State == proto.HealthPassing {
		r.log.InfoContext(ctx, "healthcheck bien", attrs...)
		return
	}
	r.log.WarnContext(ctx, "healthcheck mal", append(attrs, "detalle", h.Detail)...)
}

// signature resume lo que muestra una foto, sin la marca de tiempo. De cada
// healthcheck cuenta solo si está bien o mal: la hora, la latencia y el
// detalle viajan con el reenvío de los 5 minutos (ADR-012).
func signature(s proto.ProjectsState) [sha256.Size]byte {
	s.TakenAt = time.Time{}
	s.Projects = slices.Clone(s.Projects)
	for i, p := range s.Projects {
		if p.Health != nil {
			s.Projects[i].Health = &proto.HealthResult{State: p.Health.State}
		}
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return [sha256.Size]byte{}
	}
	return sha256.Sum256(raw)
}
