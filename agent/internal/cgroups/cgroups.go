// Package cgroups lee lo que consume cada contenedor y cada servicio desde
// cgroup v2, y dice a quién pertenece un proceso según su cgroup.
//
// Es un solo mecanismo para los tres adaptadores del agent (ADR-011): Docker y
// systemd ponen cada contenedor y cada servicio en su propio cgroup, y el
// kernel ya lleva la cuenta de su CPU y su memoria. Leerla son dos o tres
// archivos chicos por unidad, sin preguntarle nada a ningún demonio.
package cgroups

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"
)

// Root es donde el kernel monta la jerarquía unificada de cgroup v2.
const Root = "/sys/fs/cgroup"

// ErrNotFound indica que el cgroup no existe: la unidad no está corriendo, o
// terminó entre que se listó y se midió.
var ErrNotFound = errors.New("el cgroup no existe")

// Stat es el consumo acumulado de un cgroup en el momento de leerlo.
//
// CPUUsage es tiempo de CPU sumado desde que se creó el cgroup: el porcentaje
// sale de restar dos lecturas (ver el paquete usage).
//
// MemoryBytes es la memoria en uso sin la caché de archivos inactiva, que el
// kernel devuelve apenas alguien la necesita. Es el mismo número que muestra
// `docker stats`, y el que dice cuánto ocupa de verdad un proyecto.
type Stat struct {
	CPUUsage    time.Duration
	MemoryBytes uint64
}

// Reader lee cgroups de una jerarquía v2.
type Reader struct {
	// fsys es la raíz de la jerarquía. En producción, os.DirFS(Root); en los
	// tests, archivos de muestra. Como fs.FS no admite rutas con "..", un
	// nombre de cgroup mal formado no puede leer fuera de la jerarquía.
	fsys fs.FS
}

// NewReader arma un lector sobre la jerarquía que recibe.
func NewReader(fsys fs.FS) *Reader {
	return &Reader{fsys: fsys}
}

// Available dice si la jerarquía es cgroup v2. En un servidor con cgroup v1 el
// agent sigue mostrando el estado de cada unidad, pero no su consumo.
func (r *Reader) Available() bool {
	_, err := fs.Stat(r.fsys, "cgroup.controllers")
	return err == nil
}

// Read lee el consumo del cgroup group, con la ruta como la escribe el kernel
// en /proc/<pid>/cgroup (por ejemplo "/system.slice/nginx.service").
func (r *Reader) Read(group string) (Stat, error) {
	dir, err := fsPath(group)
	if err != nil {
		return Stat{}, err
	}

	cpu, err := r.readCPU(dir)
	if err != nil {
		return Stat{}, err
	}
	memory, err := r.readMemory(dir)
	if err != nil {
		return Stat{}, err
	}
	return Stat{CPUUsage: cpu, MemoryBytes: memory}, nil
}

// ContainerGroup busca el cgroup de un contenedor. Depende del driver de
// cgroups de Docker: con systemd, que es el de cualquier VPS con systemd, es
// /system.slice/docker-<id>.scope; con cgroupfs, que usa por ejemplo Docker
// Desktop, es /docker/<id>.
func (r *Reader) ContainerGroup(id string) (string, bool) {
	for _, group := range []string{
		"/system.slice/docker-" + id + ".scope",
		"/docker/" + id,
	} {
		dir, err := fsPath(group)
		if err != nil {
			return "", false
		}
		if _, err := fs.Stat(r.fsys, path.Join(dir, "cpu.stat")); err == nil {
			return group, true
		}
	}
	return "", false
}

func (r *Reader) readCPU(dir string) (time.Duration, error) {
	data, err := r.readFile(dir, "cpu.stat")
	if err != nil {
		return 0, err
	}
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), " ")
		if !ok || key != "usage_usec" {
			continue
		}
		usec, err := strconv.ParseUint(value, 10, 63)
		if err != nil {
			return 0, fmt.Errorf("leer usage_usec de %s: %w", dir, err)
		}
		return time.Duration(usec) * time.Microsecond, nil
	}
	return 0, fmt.Errorf("%s/cpu.stat no trae usage_usec", dir)
}

func (r *Reader) readMemory(dir string) (uint64, error) {
	data, err := r.readFile(dir, "memory.current")
	if err != nil {
		return 0, err
	}
	current, err := strconv.ParseUint(strings.TrimSpace(data), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("leer memory.current de %s: %w", dir, err)
	}

	// Sin memory.stat queda la memoria con la caché incluida: sobrestima, pero
	// es mejor que no mostrar nada.
	stat, err := r.readFile(dir, "memory.stat")
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return current, nil
		}
		return 0, err
	}
	scanner := bufio.NewScanner(strings.NewReader(stat))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), " ")
		if !ok || key != "inactive_file" {
			continue
		}
		inactive, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("leer inactive_file de %s: %w", dir, err)
		}
		if inactive > current {
			return 0, nil
		}
		return current - inactive, nil
	}
	return current, nil
}

func (r *Reader) readFile(dir, name string) (string, error) {
	data, err := fs.ReadFile(r.fsys, path.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w: %s", ErrNotFound, dir)
	}
	if err != nil {
		return "", fmt.Errorf("leer %s/%s: %w", dir, name, err)
	}
	return string(data), nil
}

// fsPath convierte la ruta de un cgroup en una ruta de fs.FS. La raíz de la
// jerarquía es ".".
func fsPath(group string) (string, error) {
	if !strings.HasPrefix(group, "/") {
		return "", fmt.Errorf("ruta de cgroup inválida %q: tiene que empezar con /", group)
	}
	dir := strings.TrimPrefix(path.Clean(group), "/")
	if dir == "" {
		return ".", nil
	}
	if !fs.ValidPath(dir) {
		return "", fmt.Errorf("ruta de cgroup inválida %q", group)
	}
	return dir, nil
}

// ParseProcCgroup saca la ruta de cgroup v2 del contenido de /proc/<pid>/cgroup,
// que es la línea que empieza con "0::". En un sistema con cgroup v1 no hay
// tal línea y devuelve false.
func ParseProcCgroup(data string) (string, bool) {
	for line := range strings.Lines(data) {
		group, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "0::")
		if ok && strings.HasPrefix(group, "/") {
			return group, true
		}
	}
	return "", false
}

// Kind es a quién pertenece un proceso.
type Kind int

const (
	// KindSystem es el propio sistema: la raíz de la jerarquía (hilos del
	// kernel) y systemd. No se muestra.
	KindSystem Kind = iota
	// KindContainer es un contenedor de Docker; el ID es el del contenedor.
	KindContainer
	// KindService es un servicio de systemd del sistema; el ID es la unidad.
	KindService
	// KindUnmanaged es todo lo demás: lo que alguien arrancó a mano en una
	// sesión SSH, un tmux, un systemd-run. Es lo que el panel muestra como
	// procesos no gestionados.
	KindUnmanaged
)

// Owner es el dueño de un cgroup.
type Owner struct {
	Kind Kind
	ID   string
}

// Classify dice a quién pertenece un cgroup.
//
// Los servicios cuentan solo bajo /system.slice: un servicio del systemd de un
// usuario (/user.slice/.../user@1000.service/...) lo arrancó una persona, no
// el sistema, y se muestra como no gestionado.
func Classify(group string) Owner {
	group = path.Clean(group)
	if group == "/" || group == "." || group == "/init.scope" {
		return Owner{Kind: KindSystem}
	}

	segments := strings.Split(strings.TrimPrefix(group, "/"), "/")
	for i, segment := range segments {
		if id, ok := strings.CutPrefix(segment, "docker-"); ok {
			if id, ok = strings.CutSuffix(id, ".scope"); ok && isContainerID(id) {
				return Owner{Kind: KindContainer, ID: id}
			}
		}
		if segment == "docker" && i+1 < len(segments) && isContainerID(segments[i+1]) {
			return Owner{Kind: KindContainer, ID: segments[i+1]}
		}
	}

	if segments[0] == "system.slice" {
		for _, segment := range segments[1:] {
			if strings.HasSuffix(segment, ".service") && segment != ".service" {
				return Owner{Kind: KindService, ID: segment}
			}
		}
	}
	return Owner{Kind: KindUnmanaged}
}

func isContainerID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
