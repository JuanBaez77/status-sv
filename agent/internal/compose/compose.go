// Package compose opera un proyecto de Docker Compose declarado: compilar,
// levantar, correr los pasos declarados y guardar o recuperar las imágenes
// que hacen falta para volver atrás (ADR-015).
//
// docker compose se ejecuta con argumentos fijos, sin shell y con un entorno
// armado desde cero, como git. Lo que se puede resolver por la API de Docker
// (qué contenedores tiene el proyecto, con qué imagen exacta, poner o sacar
// etiquetas) se hace con el cliente propio del agent, sin ejecutar nada.
package compose

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/docker"
	"github.com/JuanBaez77/status-sv/agent/internal/projectspec"
	"github.com/JuanBaez77/status-sv/agent/internal/runner"
	"github.com/JuanBaez77/status-sv/proto"
)

// Tiempos máximos (ADR-015). Los de los pasos declarados vienen del archivo.
const (
	BuildTimeout = 15 * time.Minute
	UpTimeout    = 5 * time.Minute
	queryTimeout = 30 * time.Second
)

// RollbackRepoPrefix es el repositorio de las imágenes que se guardan para
// volver atrás. Es propio: no se confunde con ninguna imagen del proyecto.
const RollbackRepoPrefix = "statussv-rollback/"

// ErrMissingImage es que la imagen a la que había que volver ya no existe
// (alguien corrió docker image prune -a).
var ErrMissingImage = errors.New("la imagen para volver atrás ya no existe en el servidor")

// Config es lo que el agent sabe de antemano, igual para todos los proyectos.
type Config struct {
	// Docker es la ruta absoluta de la CLI de docker.
	Docker string
	// Socket es el socket de Docker.
	Socket string
	// StateDir es el directorio de estado del agent: HOME y DOCKER_CONFIG.
	StateDir string
}

// Project es un proyecto de Compose declarado.
type Project struct {
	cfg     Config
	name    string
	workdir string
	file    string
}

// New prepara un proyecto. Solo acepta proyectos de Docker válidos.
func New(cfg Config, p proto.Project) (*Project, error) {
	if p.Runtime != proto.RuntimeDocker {
		return nil, fmt.Errorf("el proyecto %s no es de Docker", p.Name)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("proyecto inválido: %w", err)
	}
	if !strings.HasPrefix(cfg.Docker, "/") && !strings.Contains(cfg.Docker, ":") {
		return nil, fmt.Errorf("docker tiene que ser una ruta absoluta: %q", cfg.Docker)
	}
	return &Project{cfg: cfg, name: p.ComposeProject, workdir: p.Workdir, file: path.Join(p.Workdir, p.ComposeFile)}, nil
}

// Name es el nombre del proyecto de Compose.
func (p *Project) Name() string { return p.name }

// command arma una invocación de docker compose sobre el proyecto. El
// directorio, el archivo y el nombre van siempre explícitos: nada depende de
// desde dónde se corra ni de variables de entorno.
func (p *Project) command(timeout time.Duration, args ...string) runner.Command {
	full := append([]string{
		"compose", "--ansi", "never",
		"--project-directory", p.workdir,
		"--file", p.file,
		"--project-name", p.name,
	}, args...)
	return runner.Command{Path: p.cfg.Docker, Args: full, Dir: p.workdir, Env: p.env(), Timeout: timeout}
}

func (p *Project) env() []string {
	return append(runner.BaseEnv(p.cfg.StateDir),
		"DOCKER_CONFIG="+path.Join(p.cfg.StateDir, "docker"),
		"DOCKER_HOST=unix://"+p.cfg.Socket,
		// Salida de build renglón por renglón, sin barras que se redibujan.
		"BUILDKIT_PROGRESS=plain",
	)
}

// Version devuelve la versión de Compose. Sirve de chequeo previo: si falta
// el plugin, el deploy no arranca.
func (p *Project) Version(ctx context.Context) (string, error) {
	c := runner.Command{Path: p.cfg.Docker, Args: []string{"compose", "version", "--short"}, Env: p.env(), Timeout: queryTimeout}
	out, err := runner.Output(ctx, c, nil)
	if err != nil {
		return "", fmt.Errorf("no está el plugin de Docker Compose: %w", err)
	}
	return runner.FirstLine(out), nil
}

// Validate lee el archivo de Compose sin ejecutar nada, para fallar antes de
// tocar algo si está roto.
func (p *Project) Validate(ctx context.Context, tail *runner.Tail) error {
	return runner.Run(ctx, p.command(queryTimeout, "config", "--quiet"), tail)
}

// Build compila las imágenes del proyecto. Lo que corre no se toca.
func (p *Project) Build(ctx context.Context, tail *runner.Tail) error {
	return runner.Run(ctx, p.command(BuildTimeout, "build"), tail)
}

// Up levanta el proyecto con las imágenes que ya hay, sin compilar: si falta
// una, falla en vez de compilar a escondidas.
func (p *Project) Up(ctx context.Context, tail *runner.Tail) error {
	return runner.Run(ctx, p.command(UpTimeout, "up", "--detach", "--no-build"), tail)
}

// Run corre un paso declarado en un contenedor de una sola vez (ADR-016), con
// la imagen recién compilada.
func (p *Project) Run(ctx context.Context, step projectspec.Step, tail *runner.Tail) error {
	if len(step.Command) == 0 || strings.HasPrefix(step.Command[0], "-") || strings.HasPrefix(step.Service, "-") {
		return fmt.Errorf("paso inválido")
	}
	args := append([]string{"run", "--rm", "--no-TTY", step.Service}, step.Command...)
	return runner.Run(ctx, p.command(step.Timeout, args...), tail)
}

// ImageAPI es lo que se usa de la API de Docker. La cumple docker.Client.
type ImageAPI interface {
	ProjectContainers(ctx context.Context, project string) ([]docker.Container, error)
	Inspect(ctx context.Context, id string) (docker.Inspection, error)
	ImageOf(ctx context.Context, id string) (docker.ContainerImage, error)
	ImageExists(ctx context.Context, ref string) (bool, error)
	Tag(ctx context.Context, id, repo, tag string) error
	Untag(ctx context.Context, ref string) error
	ImagesByReference(ctx context.Context, pattern string) ([]string, error)
}

// ServiceImage es la imagen exacta con la que corre un servicio: su ID y la
// referencia que dice el compose. Es lo que se anota antes de desplegar.
type ServiceImage struct {
	Service string `json:"service"`
	ID      string `json:"id"`
	Ref     string `json:"ref"`
}

// Snapshot anota la imagen de cada servicio del proyecto que tiene un
// contenedor. Un servicio con varias réplicas aparece una vez.
func (p *Project) Snapshot(ctx context.Context, api ImageAPI) ([]ServiceImage, error) {
	containers, err := api.ProjectContainers(ctx, p.name)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var images []ServiceImage
	for _, ctr := range containers {
		service := docker.ServiceOf(ctr)
		if service == "" || seen[service] {
			continue
		}
		img, err := api.ImageOf(ctx, ctr.ID)
		if err != nil {
			return nil, err
		}
		seen[service] = true
		images = append(images, ServiceImage{Service: service, ID: img.ID, Ref: img.Ref})
	}
	slices.SortFunc(images, func(a, b ServiceImage) int { return strings.Compare(a.Service, b.Service) })
	return images, nil
}

// Retain etiqueta las imágenes anotadas con el commit al que pertenecen, así
// ningún build nuevo ni un prune de imágenes sin etiqueta se las lleva.
func (p *Project) Retain(ctx context.Context, api ImageAPI, commit string, images []ServiceImage) error {
	if !proto.ValidCommit(commit) {
		return fmt.Errorf("commit inválido")
	}
	for _, img := range images {
		if err := api.Tag(ctx, img.ID, RollbackRepo(p.name, img.Service), ShortCommit(commit)); err != nil {
			return fmt.Errorf("guardar la imagen de %s: %w", img.Service, err)
		}
	}
	return nil
}

// Restore vuelve a apuntar la referencia de cada servicio a su imagen
// anotada, para que un up sin compilar recree con la anterior. Revisa antes
// que existan todas: si falta una, no toca nada.
func (p *Project) Restore(ctx context.Context, api ImageAPI, images []ServiceImage) error {
	for _, img := range images {
		ok, err := api.ImageExists(ctx, img.ID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w (%s)", ErrMissingImage, img.Service)
		}
	}
	for _, img := range images {
		repo, tag, ok := docker.SplitRef(img.Ref)
		if !ok {
			// Una imagen por digest no se mueve: el compose del commit
			// anterior ya la nombra exacta.
			continue
		}
		if err := api.Tag(ctx, img.ID, repo, tag); err != nil {
			return fmt.Errorf("volver a la imagen anterior de %s: %w", img.Service, err)
		}
	}
	return nil
}

// Prune saca las etiquetas de rollback del proyecto que no son de los commits
// en keep. Si una etiqueta era lo único que retenía a una imagen, Docker la
// borra; las que usa algún contenedor quedan.
func (p *Project) Prune(ctx context.Context, api ImageAPI, keep []string) error {
	prefix := RollbackRepoPrefix + repoComponent(p.name) + "/"
	refs, err := api.ImagesByReference(ctx, prefix+"*")
	if err != nil {
		return err
	}
	keepTags := make(map[string]bool, len(keep))
	for _, c := range keep {
		keepTags[ShortCommit(c)] = true
	}
	var errs []error
	for _, ref := range refs {
		repo, tag, ok := docker.SplitRef(ref)
		if !ok || !strings.HasPrefix(repo, prefix) || keepTags[tag] {
			continue
		}
		if err := api.Untag(ctx, ref); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RollbackRepo es el repositorio donde se guarda la imagen de un servicio:
// statussv-rollback/<proyecto>/<servicio>. La barra separa sin ambigüedad: con
// un guion, las de "tienda" y las de "tienda-vieja" se confundirían.
func RollbackRepo(project, service string) string {
	return RollbackRepoPrefix + repoComponent(project) + "/" + repoComponent(service)
}

// ShortCommit es la etiqueta de un commit: sus primeros 12 caracteres.
func ShortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

var notRepoChars = regexp.MustCompile(`[^a-z0-9]+`)

// repoComponent deja un nombre de proyecto o de servicio como componente
// válido de un repositorio de Docker: minúsculas, números y guiones sueltos.
func repoComponent(s string) string {
	return strings.Trim(notRepoChars.ReplaceAllString(strings.ToLower(s), "-"), "-")
}
