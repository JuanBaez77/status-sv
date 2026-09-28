// Package projects lee los proyectos declarados en el servidor y decide cuándo
// mandarlos al control plane (ADR-012).
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
	"github.com/JuanBaez77/status-sv/agent/internal/projectspec"
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

// readOne lee un archivo de proyecto. Solo acepta archivos regulares: un
// enlace simbólico podría apuntar a cualquier archivo que el agent pueda leer.
func readOne(path string, entry fs.DirEntry) (proto.Project, error) {
	if !entry.Type().IsRegular() {
		return proto.Project{}, errors.New("no es un archivo regular (los enlaces simbólicos no se siguen)")
	}
	f, err := os.Open(path) //nolint:gosec // la ruta sale del directorio de proyectos, que es de root
	if err != nil {
		return proto.Project{}, errors.New("no se puede abrir")
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, projectspec.MaxFileBytes+1))
	if err != nil {
		return proto.Project{}, errors.New("no se puede leer")
	}
	return projectspec.Parse(filepath.Base(path), data)
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

// Runner relee el directorio y deja en la bandeja las fotos que cambiaron.
type Runner struct {
	dir    string
	outbox *Outbox
	log    *slog.Logger
	now    func() time.Time

	refresh, resend time.Duration

	mu       sync.Mutex
	current  proto.ProjectsState
	lastSig  [sha256.Size]byte
	lastSent time.Time
}

// NewRunner arma el Runner sobre un directorio.
func NewRunner(dir string, o *Outbox, log *slog.Logger) *Runner {
	return &Runner{
		dir:     dir,
		outbox:  o,
		log:     log,
		now:     time.Now,
		refresh: DefaultRefresh,
		resend:  DefaultResend,
	}
}

// Run relee el directorio hasta que se cancela ctx: una vez al arrancar y
// después en cada refresco.
func (r *Runner) Run(ctx context.Context) {
	r.take(ctx)
	ticker := time.NewTicker(r.refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.take(ctx)
		}
	}
}

// Current devuelve los proyectos de la última lectura.
func (r *Runner) Current() []proto.Project {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.current.Projects)
}

func (r *Runner) take(ctx context.Context) {
	state := Read(r.dir, r.now())
	sig := signature(state)

	r.mu.Lock()
	r.current = state
	unchanged := sig == r.lastSig && r.now().Sub(r.lastSent) < r.resend
	if !unchanged {
		r.lastSig, r.lastSent = sig, r.now()
	}
	r.mu.Unlock()
	if unchanged {
		return
	}

	r.outbox.Put(state)
	r.log.InfoContext(ctx, "proyectos declarados leídos",
		"directorio", r.dir,
		"fuente", state.Source.Status,
		"validos", len(state.Projects),
		"invalidos", len(state.Invalid),
	)
	for _, invalid := range state.Invalid {
		r.log.WarnContext(ctx, "archivo de proyecto inválido", "archivo", invalid.File, "error", invalid.Error)
	}
}

// signature resume lo que muestra una foto, sin la marca de tiempo.
func signature(s proto.ProjectsState) [sha256.Size]byte {
	s.TakenAt = time.Time{}
	raw, err := json.Marshal(s)
	if err != nil {
		return [sha256.Size]byte{}
	}
	return sha256.Sum256(raw)
}
