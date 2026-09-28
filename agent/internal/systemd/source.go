// Package systemd lee los servicios de systemd por D-Bus, en solo lectura.
//
// Habla con org.freedesktop.systemd1 por el bus del sistema, que no pide
// privilegios para listar unidades ni leer sus propiedades (ADR-011). No
// ejecuta systemctl: la misma información llega tipada y sin procesos hijos.
package systemd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"

	"github.com/JuanBaez77/status-sv/proto"
)

// DefaultExclude son los servicios que no se muestran: la maquinaria del propio
// sistema, que no dice nada de los proyectos alojados y llenaría la lista. Son
// patrones de path.Match sobre el nombre de la unidad.
var DefaultExclude = []string{
	"systemd-*",
	"user@*",
	"user-runtime-dir@*",
	"dbus.service",
	"dbus-broker.service",
	"getty@*",
	"serial-getty@*",
	"console-getty.service",
	"modprobe@*",
	"plymouth*",
	"initrd-*",
	"emergency.service",
	"rescue.service",
	"kmod-static-nodes.service",
}

// Espera entre intentos de reabrir la suscripción a las señales de systemd.
const (
	watchRetryFirst = time.Second
	watchRetryCap   = time.Minute
	// watchCheckEvery es cada cuánto Watch mira que la conexión siga viva: si
	// el bus se reinicia, las señales dejan de llegar sin ningún error.
	watchCheckEvery = 30 * time.Second
)

// api es la parte de *dbus.Conn que usa la fuente. Los tests la reemplazan.
type api interface {
	ListUnitsByPatternsContext(ctx context.Context, states, patterns []string) ([]dbus.UnitStatus, error)
	ListUnitFilesByPatternsContext(ctx context.Context, states, patterns []string) ([]dbus.UnitFile, error)
	GetServicePropertyContext(ctx context.Context, service, propertyName string) (*dbus.Property, error)
	GetUnitPropertyContext(ctx context.Context, unit, propertyName string) (*dbus.Property, error)
	Subscribe() error
	SetPropertiesSubscriber(updateCh chan<- *dbus.PropertiesUpdate, errCh chan<- error)
	Connected() bool
	Close()
}

var _ api = (*dbus.Conn)(nil)

// Unit es un servicio con el cgroup donde corre, que es de donde sale su
// consumo. El cgroup no viaja en el contrato: lo usa el agent.
type Unit struct {
	Service      proto.Service
	ControlGroup string
}

// Source es la fuente "systemd" del estado de runtime.
//
// Snapshot y Watch corren en goroutines distintas y cada una tiene su propia
// conexión al bus; comparten el caché de detalles, que va con candado.
type Source struct {
	dial    func(ctx context.Context) (api, error)
	log     *slog.Logger
	exclude []string

	// conn es la conexión de Snapshot. Se abre al primer uso y se tira ante
	// cualquier error, para reabrirla en la foto siguiente.
	conn api

	mu sync.Mutex
	// details guarda lo que cuesta un pedido por propiedad (PID principal,
	// cgroup y desde cuándo está activo), para no pedirlo en cada foto. Se
	// invalida cuando cambia el estado de la unidad o llega una señal suya.
	details map[string]detail
	// types guarda el tipo de cada servicio (simple, oneshot...), que solo
	// cambia si se recarga la unidad.
	types map[string]string

	// retryFirst y retryCap son la primera espera y el tope entre intentos de
	// reabrir la suscripción. Los tests los achican, igual que checkEvery.
	retryFirst, retryCap, checkEvery time.Duration
}

type detail struct {
	active, sub  string
	mainPID      int
	controlGroup string
	activeSince  time.Time
	// result es cómo terminó la última vez; solo de lo parado o caído.
	result string
	// conditionFailed es un servicio parado que ni arrancó porque su
	// condición no se cumple en este servidor (un ConditionPathExists, por
	// ejemplo): no está pensado para correr acá.
	conditionFailed bool
}

// NewSource arma la fuente sobre el bus del sistema. exclude son patrones de
// path.Match de los servicios que no se muestran; nil usa DefaultExclude.
func NewSource(log *slog.Logger, exclude []string) *Source {
	if exclude == nil {
		exclude = DefaultExclude
	}
	return newSource(func(ctx context.Context) (api, error) {
		return dbus.NewSystemConnectionContext(ctx)
	}, log, exclude)
}

func newSource(dial func(context.Context) (api, error), log *slog.Logger, exclude []string) *Source {
	return &Source{
		dial:       dial,
		log:        log,
		exclude:    exclude,
		details:    make(map[string]detail),
		types:      make(map[string]string),
		retryFirst: watchRetryFirst,
		retryCap:   watchRetryCap,
		checkEvery: watchCheckEvery,
	}
}

// Close cierra la conexión de Snapshot, si hay una abierta.
func (s *Source) Close() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

// Snapshot lista los servicios que se muestran y dice si la fuente se pudo
// leer. No es seguro llamarla desde varias goroutines a la vez.
//
// Qué servicios se muestran lo decide shown.
func (s *Source) Snapshot(ctx context.Context) ([]Unit, proto.RuntimeSource) {
	if s.conn == nil {
		conn, err := s.dial(ctx)
		if err != nil {
			return nil, sourceFromDialError(err)
		}
		s.conn = conn
	}

	units, err := s.list(ctx)
	if err != nil {
		s.Close()
		return nil, proto.SourceFailed(err)
	}
	return units, proto.RuntimeSource{Status: proto.SourceOK}
}

func (s *Source) list(ctx context.Context) ([]Unit, error) {
	statuses, err := s.conn.ListUnitsByPatternsContext(ctx, nil, []string{"*.service"})
	if err != nil {
		return nil, fmt.Errorf("listar servicios: %w", err)
	}
	files, err := s.conn.ListUnitFilesByPatternsContext(ctx, []string{"enabled"}, []string{"*.service"})
	if err != nil {
		return nil, fmt.Errorf("listar servicios habilitados: %w", err)
	}
	enabled := make(map[string]bool, len(files))
	for _, f := range files {
		enabled[path.Base(f.Path)] = true
	}

	units := make([]Unit, 0, len(statuses))
	// seen son los que pasaron el filtro y tienen sus detalles en caché;
	// listed, todos los que existen. El tipo se recuerda también de los que se
	// esconden, para no volver a preguntarlo.
	seen := make(map[string]bool, len(statuses))
	listed := make(map[string]bool, len(statuses))
	for _, st := range statuses {
		listed[st.Name] = true
		if !s.shown(ctx, st, enabled) {
			continue
		}
		seen[st.Name] = true
		// Si falla un detalle (la unidad pudo desaparecer entre la lista y la
		// consulta), el servicio se muestra igual, sin PID ni cgroup, y se
		// vuelve a preguntar en la foto siguiente: no cae la fuente entera.
		d, err := s.detailFor(ctx, st)
		if err != nil {
			s.log.DebugContext(ctx, "no se pudieron leer los detalles de un servicio", "unidad", st.Name, "error", err)
		}
		if d.conditionFailed {
			continue
		}
		units = append(units, Unit{
			Service: proto.Service{
				Unit:        st.Name,
				LoadState:   st.LoadState,
				ActiveState: activeState(st.ActiveState),
				SubState:    st.SubState,
				Result:      d.result,
				MainPID:     d.mainPID,
				ActiveSince: d.activeSince,
			},
			ControlGroup: d.controlGroup,
		})
	}

	s.mu.Lock()
	for name := range s.types {
		if !listed[name] {
			delete(s.types, name)
		}
	}
	for name := range s.details {
		if !seen[name] {
			delete(s.details, name)
		}
	}
	s.mu.Unlock()
	return units, nil
}

// shown decide si un servicio se muestra. Se muestran tres cosas:
//
//   - lo que está corriendo o cambiando de estado;
//   - lo que falló, siempre;
//   - lo que está habilitado pero parado: arranca con el sistema y no está
//     corriendo. Cómo terminó (Result) le dice al panel si es un problema.
//
// Se esconde el resto, que es ruido para quien mira los proyectos de un
// servidor: los oneshot que ya terminaron (configuración de arranque, tareas
// de apt), estén "active (exited)" o inactivos, y lo parado que no está
// habilitado. Lo parado cuya condición no se cumple en este servidor se
// esconde después, en list, porque saberlo cuesta un pedido por unidad.
func (s *Source) shown(ctx context.Context, st dbus.UnitStatus, enabled map[string]bool) bool {
	for _, pattern := range s.exclude {
		if ok, _ := path.Match(pattern, st.Name); ok {
			return false
		}
	}
	switch {
	case st.ActiveState == "failed":
		return true
	case st.ActiveState == "inactive":
		// Una unidad nombrada por otra pero sin archivo no está habilitada,
		// así que tampoco pasa por acá.
		return enabled[st.Name] && !s.oneshot(ctx, st.Name)
	case st.SubState == "exited":
		// Un oneshot con RemainAfterExit: corrió al arrancar y terminó bien.
		return false
	default:
		return true
	}
}

// oneshot dice si el servicio es de tipo oneshot. El tipo no cambia mientras
// no se recargue la unidad, así que se pregunta una vez. Si la pregunta falla,
// se lo muestra: es peor esconder un servicio que mostrar uno de más.
func (s *Source) oneshot(ctx context.Context, unit string) bool {
	s.mu.Lock()
	cached, ok := s.types[unit]
	s.mu.Unlock()
	if ok {
		return cached == "oneshot"
	}

	value, err := s.serviceProperty(ctx, unit, "Type")
	if err != nil {
		s.log.DebugContext(ctx, "no se pudo leer el tipo de un servicio", "unidad", unit, "error", err)
		return false
	}
	serviceType, _ := value.(string)
	s.mu.Lock()
	s.types[unit] = serviceType
	s.mu.Unlock()
	return serviceType == "oneshot"
}

// detailFor devuelve lo que no viene en la lista de unidades, del caché si el
// estado no cambió o preguntándole a systemd. De lo que corre, el PID
// principal, el cgroup y desde cuándo está activo; de lo parado o caído, cómo
// terminó y, si está parado, si su condición no se cumple.
func (s *Source) detailFor(ctx context.Context, st dbus.UnitStatus) (detail, error) {
	s.mu.Lock()
	cached, ok := s.details[st.Name]
	s.mu.Unlock()
	if ok && cached.active == st.ActiveState && cached.sub == st.SubState {
		return cached, nil
	}

	d := detail{active: st.ActiveState, sub: st.SubState}
	switch st.ActiveState {
	case "inactive", "failed":
		result, err := s.serviceProperty(ctx, st.Name, "Result")
		if err != nil {
			return detail{}, err
		}
		d.result, _ = result.(string)
		if st.ActiveState == "inactive" {
			failed, err := s.conditionFailed(ctx, st.Name)
			if err != nil {
				return detail{}, err
			}
			d.conditionFailed = failed
		}
	case "active", "reloading":
		pid, err := s.serviceProperty(ctx, st.Name, "MainPID")
		if err != nil {
			return detail{}, err
		}
		if v, ok := pid.(uint32); ok {
			d.mainPID = int(v)
		}

		group, err := s.serviceProperty(ctx, st.Name, "ControlGroup")
		if err != nil {
			return detail{}, err
		}
		if v, ok := group.(string); ok {
			d.controlGroup = v
		}

		prop, err := s.conn.GetUnitPropertyContext(ctx, st.Name, "ActiveEnterTimestamp")
		if err != nil {
			return detail{}, fmt.Errorf("leer ActiveEnterTimestamp de %s: %w", st.Name, err)
		}
		// Microsegundos desde 1970; cero es "nunca".
		if v, ok := prop.Value.Value().(uint64); ok && v > 0 {
			d.activeSince = time.UnixMicro(int64(v)).UTC() //nolint:gosec // una fecha en microsegundos entra en int64 por siglos
		}
	}

	s.mu.Lock()
	s.details[st.Name] = d
	s.mu.Unlock()
	return d, nil
}

func (s *Source) serviceProperty(ctx context.Context, unit, name string) (any, error) {
	prop, err := s.conn.GetServicePropertyContext(ctx, unit, name)
	if err != nil {
		return nil, fmt.Errorf("leer %s de %s: %w", name, unit, err)
	}
	return prop.Value.Value(), nil
}

// conditionFailed dice si systemd miró las condiciones de la unidad y no se
// cumplieron. ConditionResult solo, no alcanza: vale "no" también en una
// unidad que todavía no se intentó arrancar (una que espera a otra que falló),
// y esa sí hay que verla. Por eso cuenta solo si hay fecha de la revisión.
func (s *Source) conditionFailed(ctx context.Context, unit string) (bool, error) {
	checked, err := s.conn.GetUnitPropertyContext(ctx, unit, "ConditionTimestamp")
	if err != nil {
		return false, fmt.Errorf("leer ConditionTimestamp de %s: %w", unit, err)
	}
	if at, _ := checked.Value.Value().(uint64); at == 0 {
		return false, nil
	}
	met, err := s.conn.GetUnitPropertyContext(ctx, unit, "ConditionResult")
	if err != nil {
		return false, fmt.Errorf("leer ConditionResult de %s: %w", unit, err)
	}
	ok, _ := met.Value.Value().(bool)
	return !ok, nil
}

// Watch se suscribe a las señales de systemd y llama a changed cada vez que un
// servicio cambia, y también al suscribirse: mientras no hubo suscripción pudo
// cambiar cualquier cosa. Si la conexión se cae, la reabre con espera
// creciente. Vuelve cuando se cancela ctx.
//
// changed tiene que volver enseguida: la juntada de ráfagas la hace quien
// llama.
func (s *Source) Watch(ctx context.Context, changed func()) {
	wait := s.retryFirst
	var lastErr string
	for {
		err := s.watchOnce(ctx, changed, func() { wait = s.retryFirst; lastErr = "" })
		if ctx.Err() != nil {
			return
		}
		if msg := err.Error(); msg != lastErr {
			lastErr = msg
			s.log.WarnContext(ctx, "sin señales de systemd; se reintenta", "error", err, "espera", wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, s.retryCap)
	}
}

// watchOnce atiende una suscripción hasta que se corta. Siempre devuelve un
// error.
func (s *Source) watchOnce(ctx context.Context, changed, subscribed func()) error {
	conn, err := s.dial(ctx)
	if err != nil {
		return fmt.Errorf("conectar al bus: %w", err)
	}
	defer conn.Close()

	updates := make(chan *dbus.PropertiesUpdate, 64)
	errs := make(chan error, 1)
	conn.SetPropertiesSubscriber(updates, errs)
	if err := conn.Subscribe(); err != nil {
		return fmt.Errorf("suscribirse a systemd: %w", err)
	}

	subscribed()
	s.mu.Lock()
	clear(s.details)
	s.mu.Unlock()
	changed()

	check := time.NewTicker(s.checkEvery)
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case update := <-updates:
			if strings.HasSuffix(update.UnitName, ".service") {
				s.invalidate(update.UnitName)
				changed()
			}
		case err := <-errs:
			// go-systemd avisa por acá cuando se llenó el canal de señales:
			// se perdió alguna, así que conviene mirar todo de nuevo.
			s.log.DebugContext(ctx, "se perdieron señales de systemd", "error", err)
			s.mu.Lock()
			clear(s.details)
			s.mu.Unlock()
			changed()
		case <-check.C:
			if !conn.Connected() {
				return errors.New("se cortó la conexión con el bus del sistema")
			}
		}
	}
}

func (s *Source) invalidate(unit string) {
	s.mu.Lock()
	delete(s.details, unit)
	s.mu.Unlock()
}

// sourceFromDialError distingue un servidor sin bus del sistema (un
// contenedor, por ejemplo) de un bus que existe pero no contesta.
func sourceFromDialError(err error) proto.RuntimeSource {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
		return proto.RuntimeSource{Status: proto.SourceUnavailable, Detail: "este servidor no tiene systemd con D-Bus"}
	}
	return proto.SourceFailed(fmt.Errorf("conectar al bus del sistema: %w", err))
}

func activeState(state string) proto.ActiveState {
	switch s := proto.ActiveState(state); s {
	case proto.ActiveActive, proto.ActiveReloading, proto.ActiveInactive, proto.ActiveFailed,
		proto.ActiveActivating, proto.ActiveDeactivating, proto.ActiveMaintenance, proto.ActiveRefreshing:
		return s
	default:
		return proto.ActiveUnknown
	}
}
