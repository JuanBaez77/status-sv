package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/JuanBaez77/status-sv/server/internal/config"
	"github.com/JuanBaez77/status-sv/server/internal/store"
	"github.com/JuanBaez77/status-sv/server/internal/users"
)

// runUserAdd da de alta a una persona del panel. La contraseña se pide por la
// terminal sin mostrarla, nunca por argumento: quedaría en el historial y en ps.
func runUserAdd(ctx context.Context, args []string) error {
	username, err := parseUsernameFlag("user-add", args)
	if err != nil {
		return err
	}
	password, err := readNewPassword()
	if err != nil {
		return err
	}

	return withUsers(ctx, func(service *users.Service) error {
		user, err := service.CreateUser(ctx, username, password)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Usuario %q creado.\n", user.Username)
		return nil
	})
}

// runUserPassword cambia la contraseña de una persona y cierra todas sus
// sesiones.
func runUserPassword(ctx context.Context, args []string) error {
	username, err := parseUsernameFlag("user-password", args)
	if err != nil {
		return err
	}
	password, err := readNewPassword()
	if err != nil {
		return err
	}

	return withUsers(ctx, func(service *users.Service) error {
		closed, err := service.SetPassword(ctx, username, password)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Contraseña cambiada. Sesiones cerradas: %d.\n", closed)
		return nil
	})
}

func parseUsernameFlag(name string, args []string) (string, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	username := flags.String("username", "", "nombre de usuario (obligatorio)")
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if *username == "" {
		flags.Usage()
		return "", errors.New("falta --username")
	}
	normalized := users.NormalizeUsername(*username)
	if !users.ValidUsername(normalized) {
		return "", users.ErrInvalidUsername
	}
	return normalized, nil
}

func withUsers(ctx context.Context, fn func(*users.Service) error) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pool, err := store.OpenPool(ctx, store.Config{DSN: cfg.DatabaseURL, MaxConns: 2})
	if err != nil {
		return err
	}
	defer pool.Close()
	return fn(users.NewService(pool, cfg.TOTPKey, newLogger(cfg)))
}

// readNewPassword pide la contraseña dos veces, sin eco. Si la entrada no es una
// terminal (un script, la prueba de punta a punta), lee una sola línea.
func readNewPassword() (string, error) {
	fd := int(os.Stdin.Fd()) //nolint:gosec // un descriptor de archivo siempre entra en int
	if !term.IsTerminal(fd) {
		return readPasswordLine(os.Stdin)
	}

	fmt.Fprint(os.Stderr, "Contraseña (12 caracteres o más): ")
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("leer la contraseña: %w", err)
	}
	fmt.Fprint(os.Stderr, "Repetila: ")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("leer la contraseña: %w", err)
	}
	if string(first) != string(second) {
		return "", errors.New("las contraseñas no coinciden")
	}
	return string(first), nil
}

func readPasswordLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("leer la contraseña: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// runUserTOTPReset borra el segundo factor y los códigos de recuperación de una
// persona que perdió el teléfono y los códigos, y cierra sus sesiones. Hace
// falta acceso al servidor, la misma barrera que para crear el usuario
// (ADR-013).
func runUserTOTPReset(ctx context.Context, args []string) error {
	username, err := parseUsernameFlag("user-totp-reset", args)
	if err != nil {
		return err
	}
	return withUsers(ctx, func(service *users.Service) error {
		closed, err := service.ResetTOTP(ctx, username)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Segundo factor de %q borrado. Sesiones cerradas: %d.\n"+
			"El próximo login le va a pedir darlo de alta de nuevo.\n", username, closed)
		return nil
	})
}
