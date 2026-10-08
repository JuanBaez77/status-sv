package projects

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/JuanBaez77/status-sv/internal/envfile"
	"github.com/JuanBaez77/status-sv/internal/projectspec"
	"github.com/JuanBaez77/status-sv/proto"
)

// EnvChecker pregunta qué variables requeridas faltan en el archivo de
// entorno de un proyecto systemd, que es de root: lo cumple el cliente del
// ayudante (ADR-019).
type EnvChecker interface {
	CheckEnv(ctx context.Context, project string) (missing []string, checked bool, err error)
}

// envRecheck es cada cuánto se le vuelve a preguntar al ayudante por un
// proyecto systemd: cada pregunta arranca un proceso root.
const envRecheck = 5 * time.Minute

// maxEnvFileBytes acota lo que se lee del .env de un proyecto Docker.
const maxEnvFileBytes = 1 << 20

// Details completa lo que el alta suma a cada proyecto (ADR-022): si están
// cargadas sus variables requeridas, por nombre y nunca por valor, y su llave
// pública.
type Details struct {
	ProjectsDir string
	KeysDir     string
	// Helper puede ser nil: sin él, las variables de un proyecto systemd
	// quedan como no miradas.
	Helper EnvChecker
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]envEntry
}

// envEntry es la última respuesta del ayudante para un proyecto, y con qué
// declaración se pidió.
type envEntry struct {
	hash   string
	at     time.Time
	status proto.EnvStatus
}

// NewDetails arma Details.
func NewDetails(projectsDir, keysDir string, helper EnvChecker) *Details {
	return &Details{ProjectsDir: projectsDir, KeysDir: keysDir, Helper: helper, now: time.Now, cache: make(map[string]envEntry)}
}

// Fill completa un proyecto leído.
func (d *Details) Fill(ctx context.Context, p *proto.Project) {
	if key := d.publicKey(p.Name); key != "" {
		p.DeployKey = key
	}
	spec, err := ReadSpec(d.ProjectsDir, p.Name)
	if err != nil || len(spec.RequiredEnv) == 0 {
		return
	}
	var status proto.EnvStatus
	if p.Runtime == proto.RuntimeDocker {
		status = dockerEnv(spec)
	} else {
		status = d.systemdEnv(ctx, spec, p.SpecHash)
	}
	p.Env = &status
}

// dockerEnv mira el .env del workdir, que lee docker compose y el agent
// también puede leer.
func dockerEnv(spec projectspec.Spec) proto.EnvStatus {
	status := proto.EnvStatus{Required: len(spec.RequiredEnv)}
	path := filepath.Join(spec.Project.Workdir, ".env")
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		status.Missing = spec.RequiredEnv
		return status
	case err != nil || !info.Mode().IsRegular():
		status.Unknown = true
		return status
	}
	f, err := os.Open(path) //nolint:gosec // workdir declarado por root
	if err != nil {
		status.Unknown = true
		return status
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxEnvFileBytes))
	if err != nil {
		status.Unknown = true
		return status
	}
	status.Missing = envfile.Missing(spec.RequiredEnv, data)
	return status
}

// systemdEnv le pregunta al ayudante, con la respuesta guardada unos minutos:
// cada pregunta arranca un proceso root. Si la declaración cambió, pregunta
// de nuevo.
func (d *Details) systemdEnv(ctx context.Context, spec projectspec.Spec, hash string) proto.EnvStatus {
	name := spec.Project.Name
	d.mu.Lock()
	entry, ok := d.cache[name]
	d.mu.Unlock()
	if ok && entry.hash == hash && d.now().Sub(entry.at) < envRecheck {
		return entry.status
	}

	status := proto.EnvStatus{Required: len(spec.RequiredEnv), Unknown: true}
	if d.Helper != nil {
		missing, checked, err := d.Helper.CheckEnv(ctx, name)
		if err == nil && checked {
			status = proto.EnvStatus{Required: len(spec.RequiredEnv), Missing: missing}
		}
	}
	d.mu.Lock()
	d.cache[name] = envEntry{hash: hash, at: d.now(), status: status}
	d.mu.Unlock()
	return status
}

// publicKey lee la llave pública de la deploy key del proyecto, sin el
// comentario. Es pública: es lo que se carga en GitHub.
func (d *Details) publicKey(name string) string {
	if d.KeysDir == "" || !proto.ValidProjectName(name) {
		return ""
	}
	path := filepath.Join(d.KeysDir, name+".pub")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<10 {
		return ""
	}
	data, err := os.ReadFile(path) //nolint:gosec // ruta armada con un nombre validado
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return ""
	}
	key := fields[0] + " " + fields[1]
	if !proto.ValidPublicKey(key) {
		return ""
	}
	return key
}
