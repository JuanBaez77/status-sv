// Command server es el control plane de status-sv: API REST, WebSocket del
// panel, servidor MCP y agregación de métricas.
//
// Subcomandos:
//
//	server serve                        levanta la API
//	server migrate up|down|status       aplica o revierte migraciones
//	server enroll-token --server NOMBRE crea un token de enrolamiento
//	server user-add --username NOMBRE   da de alta a una persona del panel
//	server user-password --username N   cambia su contraseña
//	server user-totp-reset --username N borra su segundo factor y cierra sus sesiones
//	server token-revoke --id ID|--all    revoca tokens del MCP
//	server version                      imprime la versión
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/JuanBaez77/status-sv/internal/buildinfo"
)

const name = "status-sv-server"

func main() {
	// El contexto se cancela con Ctrl+C o con el SIGTERM de systemd, y desde ahí
	// cada subcomando decide cómo terminar ordenadamente.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("falta el subcomando")
	}

	switch args[0] {
	case "serve":
		return runServe(ctx, args[1:])
	case "migrate":
		return runMigrate(ctx, args[1:])
	case "user-add":
		return runUserAdd(ctx, args[1:])
	case "user-password":
		return runUserPassword(ctx, args[1:])
	case "user-totp-reset":
		return runUserTOTPReset(ctx, args[1:])
	case "token-revoke":
		return runTokenRevoke(ctx, args[1:])
	case "enroll-token":
		return runEnrollToken(ctx, args[1:])
	case "version", "--version", "-version":
		fmt.Println(buildinfo.Line(name))
		return nil
	case "help", "--help", "-h":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("subcomando desconocido: %q", args[0])
	}
}

func usage(out *os.File) {
	_, _ = fmt.Fprintf(out, `%s: control plane de status-sv

Uso:
  server serve                           levanta la API
  server migrate up                      aplica las migraciones pendientes
  server migrate down                    revierte la última migración
  server migrate status                  muestra el estado de cada migración
  server enroll-token --server NOMBRE    crea un token de enrolamiento
  server user-add --username NOMBRE      da de alta a una persona del panel (pide la contraseña)
  server user-password --username NOMBRE cambia la contraseña y cierra sus sesiones
  server user-totp-reset --username NOMBRE borra el segundo factor y cierra sus sesiones:
                                          el próximo login pide darlo de alta de nuevo
  server token-revoke --id ID            revoca un token del MCP
  server token-revoke --all              revoca todos los tokens del MCP (emergencia)
  server version                         imprime la versión

Configuración (variables de entorno, ver .env.example):
  DATABASE_URL              cadena de conexión de PostgreSQL (obligatoria)
  STATUSSV_TOTP_KEY_FILE    archivo con la clave que cifra los secretos TOTP (obligatoria;
                            se genera con: openssl rand -base64 32)
  STATUSSV_LISTEN_ADDR      dónde escucha la API (por defecto 127.0.0.1:8080)
  STATUSSV_ENROLLMENT_TTL   cuánto vive un token de enrolamiento (por defecto 15m)
  STATUSSV_OFFLINE_AFTER    silencio tras el cual un servidor se marca caído (45s)
  STATUSSV_LOG_LEVEL        debug, info, warn o error (por defecto info)
  STATUSSV_LOG_FORMAT       json o text (por defecto json)
`, name)
}
