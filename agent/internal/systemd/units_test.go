package systemd

import (
	"context"
	"errors"
	"testing"

	"github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

// fakeProps contesta las propiedades de una unidad y cuenta las conexiones.
type fakeProps struct {
	values map[string]any
	err    error
	closed bool
}

func (f *fakeProps) get(name string) (*dbus.Property, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &dbus.Property{Name: name, Value: godbus.MakeVariant(f.values[name])}, nil
}

func (f *fakeProps) GetUnitPropertyContext(_ context.Context, _, name string) (*dbus.Property, error) {
	return f.get(name)
}

func (f *fakeProps) GetServicePropertyContext(_ context.Context, _, name string) (*dbus.Property, error) {
	return f.get(name)
}

func (f *fakeProps) Close() { f.closed = true }

func TestUnitReaderReadsTheState(t *testing.T) {
	conn := &fakeProps{values: map[string]any{
		"ActiveState": "active", "SubState": "running", "MainPID": uint32(4242), "NRestarts": uint32(3),
	}}
	dials := 0
	reader := &UnitReader{dial: func(context.Context) (unitProps, error) {
		dials++
		return conn, nil
	}}

	state, err := reader.UnitState(t.Context(), "gunicorn-facturas.service")
	if err != nil {
		t.Fatalf("UnitState: %v", err)
	}
	if state != (UnitState{Active: "active", Sub: "running", MainPID: 4242, Restarts: 3}) {
		t.Errorf("estado = %+v", state)
	}
	// La conexión se reusa entre una mirada y la siguiente.
	if _, err := reader.UnitState(t.Context(), "gunicorn-facturas.service"); err != nil || dials != 1 {
		t.Errorf("segunda mirada: %v, %d conexiones", err, dials)
	}
}

// Una conexión que falla se cierra y se abre de nuevo en el próximo pedido.
func TestUnitReaderReconnects(t *testing.T) {
	broken := &fakeProps{err: errors.New("conexión cerrada")}
	good := &fakeProps{values: map[string]any{"ActiveState": "active", "SubState": "running", "MainPID": uint32(1), "NRestarts": uint32(0)}}
	conns := []*fakeProps{broken, good}
	reader := &UnitReader{dial: func(context.Context) (unitProps, error) {
		next := conns[0]
		conns = conns[1:]
		return next, nil
	}}

	if _, err := reader.UnitState(t.Context(), "x.service"); err == nil {
		t.Fatal("con la conexión rota no falló")
	}
	if !broken.closed {
		t.Error("la conexión rota no se cerró")
	}
	state, err := reader.UnitState(t.Context(), "x.service")
	if err != nil || state.Active != "active" {
		t.Errorf("después de reconectar: %+v, %v", state, err)
	}

	reader.Close()
	if !good.closed {
		t.Error("Close no cerró la conexión")
	}
}

func TestUnitReaderWithoutBus(t *testing.T) {
	reader := &UnitReader{dial: func(context.Context) (unitProps, error) {
		return nil, errors.New("no hay bus")
	}}
	if _, err := reader.UnitState(t.Context(), "x.service"); err == nil {
		t.Error("sin bus no falló")
	}
}
