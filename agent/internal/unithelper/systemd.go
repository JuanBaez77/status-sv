package unithelper

import (
	"context"
	"errors"
	"fmt"

	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

// jobDone es el resultado de un trabajo de systemd que terminó bien.
const jobDone = "done"

// Systemd le habla a systemd por D-Bus. Lo usa el ayudante, como root.
type Systemd struct {
	conn *sd.Conn
}

// ConnectSystemd abre la conexión privada con systemd, la que usa systemctl
// cuando corre como root. No pasa por el bus del sistema ni por polkit, así
// que anda igual en un servidor que no los tiene.
func ConnectSystemd(ctx context.Context) (*Systemd, error) {
	conn, err := sd.NewSystemdConnectionContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("conectar con systemd: %w", err)
	}
	return &Systemd{conn: conn}, nil
}

// Close cierra la conexión.
func (s *Systemd) Close() {
	s.conn.Close()
}

// envFile es un archivo de entorno como lo espera systemd: la ruta y si se
// tolera que falte.
type envFile struct {
	Path     string
	Optional bool
}

// stepProperties son las propiedades de la unidad transitoria de un paso
// (ADR-019): de una sola vez, con el usuario del proyecto, parada en su
// workdir, con su tiempo máximo y sin poder ganar privilegios. /usr, /boot y
// /etc quedan en solo lectura; en el resto escribe lo que le permita su usuario.
func stepProperties(step Step) []sd.Property {
	props := []sd.Property{
		sd.PropDescription(step.Description),
		sd.PropType("oneshot"),
		sd.PropExecStart(step.Argv, false),
		{Name: "User", Value: dbus.MakeVariant(step.User)},
		{Name: "WorkingDirectory", Value: dbus.MakeVariant(step.Workdir)},
		{Name: "TimeoutStartUSec", Value: dbus.MakeVariant(uint64(step.Timeout.Microseconds()))}, //nolint:gosec // el tiempo de un paso es positivo y de minutos
		{Name: "NoNewPrivileges", Value: dbus.MakeVariant(true)},
		{Name: "PrivateTmp", Value: dbus.MakeVariant(true)},
		{Name: "ProtectSystem", Value: dbus.MakeVariant("full")},
	}
	if step.EnvFile != "" {
		props = append(props, sd.Property{Name: "EnvironmentFiles", Value: dbus.MakeVariant([]envFile{{Path: step.EnvFile}})})
	}
	return props
}

// RunStep arranca la unidad del paso y espera a que termine. Con una unidad
// de una sola vez, el trabajo de arranque termina cuando termina el programa.
func (s *Systemd) RunStep(ctx context.Context, step Step) (Outcome, error) {
	// Si quedó una unidad fallida con este nombre (un pedido repetido), se
	// suelta: si no, systemd diría que ya existe.
	_ = s.conn.ResetFailedUnitContext(ctx, step.Unit)

	done := make(chan string, 1)
	if _, err := s.conn.StartTransientUnitContext(ctx, step.Unit, "fail", stepProperties(step), done); err != nil {
		return Outcome{}, fmt.Errorf("arrancar %s: %w", step.Unit, err)
	}
	var job string
	select {
	case job = <-done:
	case <-ctx.Done():
		return Outcome{}, ctx.Err()
	}
	if job == jobDone {
		return Outcome{OK: true, Result: "success"}, nil
	}

	// El motivo y el código de salida se le preguntan a la unidad, que queda
	// cargada como fallida hasta que se la suelta.
	outcome := Outcome{Result: job}
	if p, err := s.conn.GetServicePropertyContext(ctx, step.Unit, "Result"); err == nil {
		if result, ok := p.Value.Value().(string); ok && result != "" {
			outcome.Result = result
		}
	}
	if p, err := s.conn.GetServicePropertyContext(ctx, step.Unit, "ExecMainStatus"); err == nil {
		if code, ok := p.Value.Value().(int32); ok {
			outcome.ExitCode = int(code)
		}
	}
	_ = s.conn.ResetFailedUnitContext(ctx, step.Unit)
	return outcome, nil
}

// Stop para una unidad y la suelta. Que no esté cargada no es un error.
func (s *Systemd) Stop(ctx context.Context, unit string) error {
	done := make(chan string, 1)
	if _, err := s.conn.StopUnitContext(ctx, unit, "replace", done); err != nil {
		if notLoaded(err) {
			return nil
		}
		return fmt.Errorf("parar %s: %w", unit, err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	_ = s.conn.ResetFailedUnitContext(ctx, unit)
	return nil
}

// Restart reinicia una unidad y espera a que el trabajo termine.
func (s *Systemd) Restart(ctx context.Context, unit string) error {
	done := make(chan string, 1)
	if _, err := s.conn.RestartUnitContext(ctx, unit, "replace", done); err != nil {
		return fmt.Errorf("reiniciar %s: %w", unit, err)
	}
	select {
	case job := <-done:
		if job != jobDone {
			return fmt.Errorf("reiniciar %s: el trabajo terminó en %s", unit, job)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Loaded dice si systemd conoce la unidad: si tiene un archivo de unidad que
// se pudo leer.
func (s *Systemd) Loaded(ctx context.Context, unit string) (bool, error) {
	p, err := s.conn.GetUnitPropertyContext(ctx, unit, "LoadState")
	if err != nil {
		return false, err
	}
	state, _ := p.Value.Value().(string)
	return state == "loaded", nil
}

// notLoaded reconoce el error de systemd para una unidad que no existe.
func notLoaded(err error) bool {
	var dbusErr dbus.Error
	return errors.As(err, &dbusErr) && dbusErr.Name == "org.freedesktop.systemd1.NoSuchUnit"
}
