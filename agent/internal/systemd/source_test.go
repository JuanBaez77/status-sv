package systemd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"

	"github.com/JuanBaez77/status-sv/proto"
)

// fakeBus imita lo que contesta systemd por D-Bus.
type fakeBus struct {
	mu        sync.Mutex
	units     []dbus.UnitStatus
	enabled   []string
	props     map[string]map[string]any
	propCalls map[string]int
	listErr   error
	connected bool
	updates   chan<- *dbus.PropertiesUpdate
	errs      chan<- error
	closed    int
}

func newFakeBus() *fakeBus {
	since := uint64(time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC).UnixMicro())
	return &fakeBus{
		units: []dbus.UnitStatus{
			{Name: "nginx.service", LoadState: "loaded", ActiveState: "active", SubState: "running"},
			{Name: "gunicorn@cristales.service", LoadState: "loaded", ActiveState: "failed", SubState: "failed"},
			// Habilitado pero parado: tiene que verse.
			{Name: "cotizador.service", LoadState: "loaded", ActiveState: "inactive", SubState: "dead"},
			// Habilitado y parado porque su condición no se cumple acá: ruido.
			{Name: "ubuntu-advantage.service", LoadState: "loaded", ActiveState: "inactive", SubState: "dead"},
			// Habilitado y parado sin que systemd haya mirado su condición
			// (espera a otra unidad que falló): tiene que verse.
			{Name: "worker.service", LoadState: "loaded", ActiveState: "inactive", SubState: "dead"},
			// Tarea de una vez, parada y no habilitada: ruido.
			{Name: "apt-daily.service", LoadState: "loaded", ActiveState: "inactive", SubState: "dead"},
			// Maquinaria del sistema: excluida por patrón.
			{Name: "systemd-journald.service", LoadState: "loaded", ActiveState: "active", SubState: "running"},
			// Oneshots de arranque que ya terminaron: ruido, estén como estén.
			{Name: "e2scrub_reap.service", LoadState: "loaded", ActiveState: "inactive", SubState: "dead"},
			{Name: "apparmor.service", LoadState: "loaded", ActiveState: "active", SubState: "exited"},
			// Nombrada por otra unidad pero sin archivo: no existe acá.
			{Name: "fantasma.service", LoadState: "not-found", ActiveState: "inactive", SubState: "dead"},
		},
		enabled: []string{
			"/etc/systemd/system/cotizador.service",
			"/lib/systemd/system/ubuntu-advantage.service",
			"/etc/systemd/system/worker.service",
			"/lib/systemd/system/nginx.service",
			"/lib/systemd/system/e2scrub_reap.service",
			"/lib/systemd/system/apparmor.service",
		},
		props: map[string]map[string]any{
			"gunicorn@cristales.service": {"Result": "exit-code"},
			"cotizador.service": {
				"Type": "simple", "Result": "success",
				"ConditionTimestamp": since, "ConditionResult": true,
			},
			"ubuntu-advantage.service": {
				"Type": "simple", "Result": "success",
				"ConditionTimestamp": since, "ConditionResult": false,
			},
			"worker.service": {
				"Type": "simple", "Result": "success",
				"ConditionTimestamp": uint64(0), "ConditionResult": false,
			},
			"e2scrub_reap.service": {"Type": "oneshot"},
			"nginx.service": {
				"MainPID":              uint32(812),
				"ControlGroup":         "/system.slice/nginx.service",
				"ActiveEnterTimestamp": since,
			},
		},
		propCalls: make(map[string]int),
		connected: true,
	}
}

func (f *fakeBus) ListUnitsByPatternsContext(_ context.Context, _, _ []string) ([]dbus.UnitStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return slices.Clone(f.units), nil
}

func (f *fakeBus) ListUnitFilesByPatternsContext(_ context.Context, _, _ []string) ([]dbus.UnitFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	files := make([]dbus.UnitFile, len(f.enabled))
	for i, p := range f.enabled {
		files[i] = dbus.UnitFile{Path: p, Type: "enabled"}
	}
	return files, nil
}

func (f *fakeBus) property(unit, name string) (*dbus.Property, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.propCalls[unit]++
	v, ok := f.props[unit][name]
	if !ok {
		return nil, fmt.Errorf("la unidad %s no tiene %s", unit, name)
	}
	return &dbus.Property{Name: name, Value: godbus.MakeVariant(v)}, nil
}

func (f *fakeBus) GetServicePropertyContext(_ context.Context, unit, name string) (*dbus.Property, error) {
	return f.property(unit, name)
}

func (f *fakeBus) GetUnitPropertyContext(_ context.Context, unit, name string) (*dbus.Property, error) {
	return f.property(unit, name)
}

func (f *fakeBus) Subscribe() error { return nil }

func (f *fakeBus) SetPropertiesSubscriber(updates chan<- *dbus.PropertiesUpdate, errs chan<- error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates, f.errs = updates, errs
}

func (f *fakeBus) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeBus) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
}

func (f *fakeBus) calls(unit string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.propCalls[unit]
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// sourceOn arma una fuente que siempre se conecta al mismo bus falso y cuenta
// las conexiones.
func sourceOn(bus *fakeBus) (*Source, *int) {
	dials := 0
	var mu sync.Mutex
	s := newSource(func(context.Context) (api, error) {
		mu.Lock()
		dials++
		mu.Unlock()
		return bus, nil
	}, quietLog(), DefaultExclude)
	return s, &dials
}

func names(units []Unit) []string {
	out := make([]string, len(units))
	for i, u := range units {
		out[i] = u.Service.Unit
	}
	return out
}

func TestSnapshotShowsWhatMatters(t *testing.T) {
	source, _ := sourceOn(newFakeBus())
	units, src := source.Snapshot(context.Background())
	if src.Status != proto.SourceOK {
		t.Fatalf("fuente: %+v", src)
	}

	want := []string{"nginx.service", "gunicorn@cristales.service", "cotizador.service", "worker.service"}
	if got := names(units); !slices.Equal(got, want) {
		t.Fatalf("servicios mostrados:\n obtuve   %v\n esperaba %v", got, want)
	}

	// Cómo terminó viaja solo en lo parado o caído.
	for i, result := range []string{"", "exit-code", "success", "success"} {
		if got := units[i].Service.Result; got != result {
			t.Errorf("%s: result %q, esperaba %q", units[i].Service.Unit, got, result)
		}
	}

	nginx := units[0]
	if nginx.Service.MainPID != 812 || nginx.ControlGroup != "/system.slice/nginx.service" ||
		!nginx.Service.ActiveSince.Equal(time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("detalles de nginx mal leídos: %+v", nginx)
	}

	// Un servicio caído o parado no tiene PID, cgroup ni hora de arranque.
	for _, u := range units[1:] {
		if u.Service.MainPID != 0 || u.ControlGroup != "" || !u.Service.ActiveSince.IsZero() {
			t.Errorf("%s no está corriendo y trae detalles: %+v", u.Service.Unit, u)
		}
	}

	for _, u := range units {
		if err := u.Service.Validate(); err != nil {
			t.Errorf("%s no pasa el contrato: %v", u.Service.Unit, err)
		}
	}
}

func TestSnapshotCachesDetails(t *testing.T) {
	bus := newFakeBus()
	source, _ := sourceOn(bus)
	ctx := context.Background()

	source.Snapshot(ctx)
	source.Snapshot(ctx)
	if n := bus.calls("nginx.service"); n != 3 {
		t.Fatalf("sin cambios, esperaba las 3 propiedades una sola vez; hubo %d pedidos", n)
	}
	// Lo que se esconde por la condición también queda en caché: si no, se
	// preguntaría de nuevo en cada foto. Tipo, resultado y las dos de la condición.
	if n := bus.calls("ubuntu-advantage.service"); n != 4 {
		t.Fatalf("esperaba 4 pedidos del servicio escondido por su condición, hubo %d", n)
	}

	bus.mu.Lock()
	bus.units[0].SubState = "reload"
	bus.units[0].ActiveState = "reloading"
	bus.mu.Unlock()
	source.Snapshot(ctx)
	if n := bus.calls("nginx.service"); n != 6 {
		t.Fatalf("con el estado cambiado esperaba 6 pedidos, hubo %d", n)
	}

	// Una señal de la unidad también invalida, aunque el estado se vea igual
	// (un reinicio rápido que vuelve a running entre dos fotos).
	source.invalidate("nginx.service")
	source.Snapshot(ctx)
	if n := bus.calls("nginx.service"); n != 9 {
		t.Fatalf("después de una señal esperaba 9 pedidos, hubo %d", n)
	}
}

func TestExcludeIsConfigurable(t *testing.T) {
	bus := newFakeBus()
	source := newSource(func(context.Context) (api, error) { return bus, nil }, quietLog(), []string{"nginx*"})
	units, _ := source.Snapshot(context.Background())
	got := names(units)
	if slices.Contains(got, "nginx.service") || !slices.Contains(got, "systemd-journald.service") {
		t.Errorf("con la exclusión propia esperaba sin nginx y con journald: %v", got)
	}
}

func TestUnknownActiveState(t *testing.T) {
	if got := activeState("hibernando"); got != proto.ActiveUnknown {
		t.Errorf("un estado nuevo de systemd tiene que ser unknown, obtuve %q", got)
	}
}

func TestDialErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want proto.SourceStatus
	}{
		{name: "sin bus del sistema", err: fmt.Errorf("dial unix /run/dbus/system_bus_socket: %w", syscall.ENOENT), want: proto.SourceUnavailable},
		{name: "bus que no contesta", err: errors.New("tiempo agotado"), want: proto.SourceError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := newSource(func(context.Context) (api, error) { return nil, tt.err }, quietLog(), nil)
			units, src := source.Snapshot(context.Background())
			if src.Status != tt.want || units != nil {
				t.Errorf("obtuve %+v con %d servicios, esperaba %q", src, len(units), tt.want)
			}
			if err := src.Validate(); err != nil {
				t.Errorf("la fuente no pasa el contrato: %v", err)
			}
		})
	}
}

// Ante un error, la conexión se tira y la foto siguiente abre otra.
func TestListErrorReconnects(t *testing.T) {
	bus := newFakeBus()
	bus.listErr = errors.New("org.freedesktop.DBus.Error.NoReply")
	source, dials := sourceOn(bus)

	if _, src := source.Snapshot(context.Background()); src.Status != proto.SourceError {
		t.Fatalf("esperaba error, obtuve %+v", src)
	}
	if bus.closed != 1 {
		t.Errorf("la conexión con error tendría que cerrarse; se cerró %d veces", bus.closed)
	}

	bus.mu.Lock()
	bus.listErr = nil
	bus.mu.Unlock()
	if _, src := source.Snapshot(context.Background()); src.Status != proto.SourceOK {
		t.Fatalf("la foto siguiente tendría que andar: %+v", src)
	}
	if *dials != 2 {
		t.Errorf("esperaba 2 conexiones, hubo %d", *dials)
	}
}

func TestWatch(t *testing.T) {
	bus := newFakeBus()
	source, dials := sourceOn(bus)
	source.retryFirst, source.retryCap, source.checkEvery = time.Millisecond, 5*time.Millisecond, 10*time.Millisecond

	source.Snapshot(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes := make(chan struct{}, 100)
	done := make(chan struct{})
	go func() {
		source.Watch(ctx, func() { changes <- struct{}{} })
		close(done)
	}()

	wait := func(what string) {
		t.Helper()
		select {
		case <-changes:
		case <-time.After(5 * time.Second):
			t.Fatalf("no llegó el aviso: %s", what)
		}
	}
	wait("al suscribirse")

	send := func(unit string) {
		bus.mu.Lock()
		updates := bus.updates
		bus.mu.Unlock()
		updates <- &dbus.PropertiesUpdate{UnitName: unit}
	}

	// Una señal de otro tipo de unidad no cuenta.
	send("nginx.socket")
	send("nginx.service")
	wait("señal de un servicio")
	select {
	case <-changes:
		t.Fatal("una señal de un socket no tendría que avisar")
	case <-time.After(50 * time.Millisecond):
	}

	// Si se cae el bus, Watch se reconecta y avisa de nuevo.
	bus.mu.Lock()
	bus.connected = false
	bus.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	bus.mu.Lock()
	bus.connected = true
	bus.mu.Unlock()
	wait("al reconectarse")
	if *dials < 3 {
		t.Errorf("esperaba al menos 3 conexiones (foto, suscripción y reconexión), hubo %d", *dials)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch no volvió al cancelar el context")
	}
}

// Si falla un detalle de un servicio, se muestra igual sin detalles, y la
// foto siguiente vuelve a preguntar en vez de quedarse con el vacío.
func TestDetailErrorKeepsTheService(t *testing.T) {
	bus := newFakeBus()
	delete(bus.props, "nginx.service")
	source, _ := sourceOn(bus)

	units, src := source.Snapshot(context.Background())
	if src.Status != proto.SourceOK || len(units) == 0 || units[0].Service.Unit != "nginx.service" {
		t.Fatalf("la fuente tendría que seguir andando con nginx: %+v, %v", src, names(units))
	}
	if units[0].Service.MainPID != 0 || units[0].ControlGroup != "" {
		t.Errorf("sin propiedades no hay detalles: %+v", units[0])
	}

	before := bus.calls("nginx.service")
	source.Snapshot(context.Background())
	if bus.calls("nginx.service") == before {
		t.Error("un detalle que falló tiene que volver a pedirse")
	}
}

// Contra el systemd de verdad, si la máquina que corre el test tiene el bus
// del sistema (el runner de CI y WSL con systemd sí). Solo lee.
func TestRealSystemd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("D-Bus del sistema es de Linux")
	}
	if _, err := os.Stat("/run/dbus/system_bus_socket"); err != nil {
		t.Skipf("no hay bus del sistema en esta máquina: %v", err)
	}

	source := NewSource(quietLog(), nil)
	defer source.Close()
	units, src := source.Snapshot(context.Background())
	if src.Status != proto.SourceOK {
		t.Fatalf("fuente: %+v", src)
	}
	running := 0
	for _, u := range units {
		if err := u.Service.Validate(); err != nil {
			t.Errorf("%s no pasa el contrato: %v", u.Service.Unit, err)
		}
		if u.Service.ActiveState == proto.ActiveActive && u.ControlGroup != "" {
			running++
		}
	}
	if running == 0 {
		t.Errorf("ningún servicio activo con cgroup entre %d: %v", len(units), names(units))
	}
	t.Logf("%d servicios, %d activos con cgroup: %v", len(units), running, names(units))
}

// El tipo de un servicio se pregunta una sola vez, aunque se esconda.
func TestServiceTypeIsAskedOnce(t *testing.T) {
	bus := newFakeBus()
	source, _ := sourceOn(bus)
	source.Snapshot(context.Background())
	source.Snapshot(context.Background())
	if n := bus.calls("e2scrub_reap.service"); n != 1 {
		t.Errorf("el tipo del oneshot se preguntó %d veces", n)
	}
	// Un "active (exited)" se esconde sin preguntar nada.
	if n := bus.calls("apparmor.service"); n != 0 {
		t.Errorf("apparmor no tendría que costar pedidos: hubo %d", n)
	}
}
