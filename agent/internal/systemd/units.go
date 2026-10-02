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

// UnitState lee el estado de una unidad de servicio.
func (r *UnitReader) UnitState(ctx context.Context, unit string) (UnitState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.conn == nil {
		conn, err := r.dial(ctx)
		if err != nil {
			return UnitState{}, fmt.Errorf("conectar al bus: %w", err)
		}
		r.conn = conn
	}
	state, err := r.read(ctx, unit)
	if err != nil {
		// Una conexión cortada no se recupera sola: se abre de nuevo en el
		// próximo pedido.
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
