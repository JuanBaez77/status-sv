package systemd

import (
	"context"
	"fmt"
	"sync"

	"github.com/coreos/go-systemd/v22/dbus"
)

// UnitState es lo que se mira de una unidad para saber si quedó andando
// después de un deploy (ADR-019).
type UnitState struct {
	Active, Sub string
	MainPID     uint32
	Restarts    uint32
}

// unitProps es la parte de *dbus.Conn que usa UnitReader. Los tests la
// reemplazan.
type unitProps interface {
	GetUnitPropertyContext(ctx context.Context, unit, propertyName string) (*dbus.Property, error)
	GetServicePropertyContext(ctx context.Context, service, propertyName string) (*dbus.Property, error)
	Close()
}

// UnitReader lee el estado de unidades puntuales por el bus del sistema, de
// solo lectura y sin privilegios. La conexión se abre con el primer pedido y
// se reabre si se corta.
type UnitReader struct {
	dial func(ctx context.Context) (unitProps, error)

	mu   sync.Mutex
	conn unitProps
}

// NewUnitReader arma un lector sobre el bus del sistema.
func NewUnitReader() *UnitReader {
	return &UnitReader{dial: func(ctx context.Context) (unitProps, error) {
		return dbus.NewSystemConnectionContext(ctx)
	}}
}

// UnitState lee el estado de una unidad de servicio. Si la conexión que
// tenía se cortó, abre otra y lo intenta una vez más.
func (r *UnitReader) UnitState(ctx context.Context, unit string) (UnitState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	reused := r.conn != nil
	state, err := r.attempt(ctx, unit)
	if err != nil && reused && ctx.Err() == nil {
		state, err = r.attempt(ctx, unit)
	}
	return state, err
}

// attempt lee con la conexión abierta, o abre una. Si la lectura falla, la
// conexión se cierra: una conexión cortada no se recupera sola.
func (r *UnitReader) attempt(ctx context.Context, unit string) (UnitState, error) {
	if r.conn == nil {
		// La conexión vive más que el pedido que la abre. La biblioteca la
		// cierra cuando se cancela el contexto con el que se abrió, y ese
		// contexto es el de un deploy: sin esto, el deploy siguiente se
		// encontraba con la conexión cerrada.
		conn, err := r.dial(context.WithoutCancel(ctx))
		if err != nil {
			return UnitState{}, fmt.Errorf("conectar al bus: %w", err)
		}
		r.conn = conn
	}
	state, err := r.read(ctx, unit)
	if err != nil {
		r.conn.Close()
		r.conn = nil
		return UnitState{}, err
	}
	return state, nil
}

func (r *UnitReader) read(ctx context.Context, unit string) (UnitState, error) {
	var state UnitState
	for name, into := range map[string]*string{"ActiveState": &state.Active, "SubState": &state.Sub} {
		prop, err := r.conn.GetUnitPropertyContext(ctx, unit, name)
		if err != nil {
			return UnitState{}, fmt.Errorf("leer %s de %s: %w", name, unit, err)
		}
		*into, _ = prop.Value.Value().(string)
	}
	for name, into := range map[string]*uint32{"MainPID": &state.MainPID, "NRestarts": &state.Restarts} {
		prop, err := r.conn.GetServicePropertyContext(ctx, unit, name)
		if err != nil {
			return UnitState{}, fmt.Errorf("leer %s de %s: %w", name, unit, err)
		}
		*into, _ = prop.Value.Value().(uint32)
	}
	return state, nil
}

// Close cierra la conexión, si hay una abierta.
func (r *UnitReader) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil {
		r.conn.Close()
		r.conn = nil
	}
}
