package docker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/JuanBaez77/status-sv/proto"
)

// Etiquetas que pone Docker Compose en cada contenedor.
const (
	labelComposeProject = "com.docker.compose.project"
	labelComposeService = "com.docker.compose.service"
)

// Espera entre intentos de reabrir el stream de eventos.
const (
	watchRetryFirst = time.Second
	watchRetryCap   = time.Minute
)

// Source es la fuente "docker" del estado de runtime: traduce lo que dice
// Docker a los tipos del contrato.
//
// Snapshot y Watch corren en goroutines distintas; comparten el caché de
// detalles, que va con candado.
type Source struct {
	client *Client
	log    *slog.Logger

	mu sync.Mutex
	// details guarda lo que sale de inspeccionar cada contenedor (cuándo
	// arrancó y su healthcheck), para no inspeccionar todos en cada foto. Se
	// invalida cuando llega un evento del contenedor o cambia su estado.
	details map[string]detail

	// retryFirst y retryCap son la primera espera y el tope entre intentos de
	// reabrir el stream de eventos. Los tests los achican.
	retryFirst, retryCap time.Duration
}

type detail struct {
	state     string
	startedAt time.Time
	health    proto.ContainerHealth
}

// NewSource arma la fuente sobre un cliente de Docker.
func NewSource(client *Client, log *slog.Logger) *Source {
	return &Source{
		client:     client,
		log:        log,
		details:    make(map[string]detail),
		retryFirst: watchRetryFirst,
		retryCap:   watchRetryCap,
	}
}

// Snapshot lista los contenedores y dice si la fuente se pudo leer.
func (s *Source) Snapshot(ctx context.Context) ([]proto.Container, proto.RuntimeSource) {
	list, err := s.client.List(ctx)
	if err != nil {
		return nil, SourceFromError(err)
	}

	containers := make([]proto.Container, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, c := range list {
		seen[c.ID] = true
		d := s.detailFor(ctx, c)
		containers = append(containers, proto.Container{
			ID:             c.ID,
			Name:           containerName(c),
			Image:          c.Image,
			State:          containerState(c.State),
			Health:         d.health,
			StartedAt:      d.startedAt,
			ComposeProject: c.Labels[labelComposeProject],
			ComposeService: c.Labels[labelComposeService],
		})
	}

	s.mu.Lock()
	for id := range s.details {
		if !seen[id] {
			delete(s.details, id)
		}
	}
	s.mu.Unlock()

	return containers, proto.RuntimeSource{Status: proto.SourceOK}
}

// detailFor devuelve los detalles del contenedor, del caché si siguen
// valiendo o inspeccionándolo. Si la inspección falla (el contenedor se borró
// en el medio, por ejemplo) quedan vacíos: es mejor mostrarlo sin la hora de
// arranque que no mostrarlo.
func (s *Source) detailFor(ctx context.Context, c Container) detail {
	s.mu.Lock()
	cached, ok := s.details[c.ID]
	s.mu.Unlock()
	if ok && cached.state == c.State {
		return cached
	}

	inspection, err := s.client.Inspect(ctx, c.ID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			s.log.DebugContext(ctx, "no se pudo inspeccionar un contenedor", "contenedor", shortID(c.ID), "error", err)
		}
		return detail{state: c.State}
	}

	d := detail{state: c.State, health: containerHealth(inspection)}
	// Docker pone el año 1 en StartedAt de un contenedor que nunca arrancó.
	if started := inspection.State.StartedAt; started.Year() > 1 {
		d.startedAt = started.UTC()
	}

	s.mu.Lock()
	s.details[c.ID] = d
	s.mu.Unlock()
	return d
}

// Watch escucha los eventos de Docker y llama a changed cada vez que algo que
// se muestra cambió, y también cada vez que el stream se abre: mientras estuvo
// cerrado pudo cambiar cualquier cosa. Si el stream se corta, lo reabre con
// una espera creciente. Vuelve cuando se cancela ctx.
//
// changed tiene que volver enseguida: la juntada de ráfagas la hace quien
// llama, no esta goroutine.
func (s *Source) Watch(ctx context.Context, changed func()) {
	wait := s.retryFirst
	var lastErr string
	for {
		err := s.client.Events(ctx,
			func() {
				wait = s.retryFirst
				lastErr = ""
				s.mu.Lock()
				clear(s.details)
				s.mu.Unlock()
				changed()
			},
			func(e Event) {
				s.invalidate(e.Actor.ID)
				changed()
			},
		)
		if ctx.Err() != nil {
			return
		}

		// Un servidor sin Docker reintentaría para siempre: se avisa una vez
		// por cada error distinto, no en cada intento.
		if msg := err.Error(); msg != lastErr {
			lastErr = msg
			level := slog.LevelWarn
			if errors.Is(err, ErrNoSocket) {
				level = slog.LevelInfo
			}
			s.log.Log(ctx, level, "sin eventos de Docker; se reintenta", "error", err, "espera", wait)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, s.retryCap)
	}
}

func (s *Source) invalidate(id string) {
	s.mu.Lock()
	delete(s.details, id)
	s.mu.Unlock()
}

// SourceFromError traduce un error del cliente al estado de la fuente, con una
// explicación que le sirva a quien mira el panel.
func SourceFromError(err error) proto.RuntimeSource {
	switch {
	case errors.Is(err, ErrNoSocket):
		return proto.RuntimeSource{Status: proto.SourceUnavailable, Detail: "Docker no está instalado en este servidor"}
	case errors.Is(err, ErrPermissionDenied):
		return proto.RuntimeSource{
			Status: proto.SourceError,
			Detail: "el agent no tiene permiso para el socket de Docker: falta el drop-in del grupo docker (ver el runbook)",
		}
	default:
		return proto.RuntimeSource{Status: proto.SourceError, Detail: sourceDetail(err)}
	}
}

// sourceDetail recorta un error para que entre en el contrato: sin caracteres
// de control y dentro del largo máximo.
func sourceDetail(err error) string {
	msg := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, err.Error())
	if len(msg) > proto.MaxSourceDetailLen {
		msg = msg[:proto.MaxSourceDetailLen]
		// No cortar un carácter de varios bytes por la mitad.
		msg = strings.ToValidUTF8(msg, "")
	}
	return msg
}

// containerName devuelve el nombre del contenedor sin la barra inicial que le
// pone Docker. Un contenedor enlazado tiene además nombres como "/otro/alias",
// que se saltean. Si no hay ninguno usable queda el ID corto, como `docker ps`.
func containerName(c Container) string {
	for _, name := range c.Names {
		name = strings.TrimPrefix(name, "/")
		if name != "" && !strings.Contains(name, "/") {
			return name
		}
	}
	return shortID(c.ID)
}

func containerState(state string) proto.ContainerState {
	switch s := proto.ContainerState(state); s {
	case proto.ContainerCreated, proto.ContainerRunning, proto.ContainerPaused, proto.ContainerRestarting,
		proto.ContainerRemoving, proto.ContainerExited, proto.ContainerDead:
		return s
	default:
		return proto.ContainerUnknown
	}
}

func containerHealth(i Inspection) proto.ContainerHealth {
	if i.State.Health == nil {
		return proto.HealthNone
	}
	switch h := proto.ContainerHealth(i.State.Health.Status); h {
	case proto.HealthStarting, proto.HealthHealthy, proto.HealthUnhealthy:
		return h
	default:
		// Docker usa "none" cuando el healthcheck está desactivado.
		return proto.HealthNone
	}
}
