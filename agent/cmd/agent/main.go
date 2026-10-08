// Command agent corre como servicio en cada VPS: se enrola una vez contra el
// control plane y después mantiene abierto el canal por el que manda su latido.
//
// Subcomandos:
//
//	agent enroll --server-url URL    canjea un token por la credencial del agent
//	agent run                        mantiene el canal abierto
//	agent sample                     imprime una muestra de métricas, sin conectarse
//	agent runtime                    imprime la foto de contenedores, servicios y procesos
//	agent deploy-check PROYECTO      revisa que un proyecto se pueda desplegar
//	agent deploy-paths               lista los proyectos que se pueden desplegar, para el instalador
//	agent deploy-dropin              imprime el drop-in 20-deploy.conf, para el instalador
//	agent project accept PROYECTO    acepta la propuesta de un proyecto nuevo (ADR-022)
//	agent project env PROYECTO       carga los valores de sus variables requeridas
//	agent project site PROYECTO      arma el sitio de nginx y pide el certificado (ADR-023)
//	agent unit-helper                atiende un pedido del agent como root (lo arranca systemd)
//	agent version                    imprime la versión
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/JuanBaez77/status-sv/agent/internal/config"
	"github.com/JuanBaez77/status-sv/internal/buildinfo"
)

const name = "status-sv-agent"

func main() {
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
	case "enroll":
		return runEnroll(ctx, args[1:])
	case "run":
		return runAgent(ctx, args[1:])
	case "runtime":
		return runRuntime(ctx, args[1:])
	case "sample":
		return runSample(ctx, args[1:])
	case "deploy-check":
		return runDeployCheck(ctx, args[1:])
	case "project":
		return runProject(ctx, args[1:])
	case "deploy-dropin":
		return runDeployDropin(ctx, args[1:])
	case "deploy-paths":
		return runDeployPaths(ctx, args[1:])
	case "unit-helper":
		return runUnitHelper(ctx, args[1:])
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
	_, _ = fmt.Fprintf(out, `%s: agent de status-sv

Uso:
  agent enroll --server-url URL     canjea un token por la credencial del agent
  agent run                         mantiene el canal abierto con el control plane
  agent sample [--interval 1s]      imprime una muestra de métricas como JSON, sin
                                    conectarse: sirve para ver qué lee el agent
  agent runtime [--interval 1s]     imprime la foto de contenedores, servicios y
                                    procesos como JSON, sin conectarse
  agent deploy-check PROYECTO       revisa, sin tocar lo que corre, que el proyecto se
                                    pueda desplegar (correrlo como el usuario del agent)
  agent deploy-dropin               imprime el drop-in 20-deploy.conf del deploy; lo usa
                                    el instalador
  agent deploy-paths                lista los proyectos que se pueden desplegar, con su
                                    workdir y su runtime; lo usa el instalador
  agent project accept PROYECTO     acepta la propuesta de un proyecto nuevo y lo declara;
                                    se corre como root (ADR-022)
  agent project env PROYECTO        carga o cambia los valores de sus variables requeridas;
                                    se corre como root (ADR-022)
  agent project site PROYECTO       arma el sitio de nginx y pide el certificado;
                                    se corre como root (ADR-023)
  agent unit-helper                 atiende un pedido del agent como root; no se corre a
                                    mano: lo arranca systemd desde su socket (ADR-019)
  agent version                     imprime la versión

Configuración (variables de entorno):
  STATUSSV_SERVER_URL      dirección del control plane
  STATUSSV_ENROLL_TOKEN    token de enrolamiento (preferido sobre --token,
                           porque los argumentos se ven con ps)
  STATUSSV_STATE_FILE      archivo de estado (por defecto %s)
  STATUSSV_LOG_LEVEL       debug, info, warn o error (por defecto info)
  STATUSSV_DOCKER_SOCKET   socket de Docker (por defecto /run/docker.sock)
  STATUSSV_SYSTEMD_EXCLUDE patrones de servicios que no se muestran, separados
                           por comas, además de los de siempre
  STATUSSV_LOG_FORMAT      json o text (por defecto json)
  STATUSSV_PROJECTS_DIR    proyectos declarados (por defecto /etc/status-sv/projects.d)
  STATUSSV_DEPLOY_ENABLED  true prende el deploy en este servidor (por defecto apagado)
  STATUSSV_DEPLOY_KEYS_DIR deploy keys, una por proyecto (por defecto /etc/status-sv/keys)
  STATUSSV_KNOWN_HOSTS     host keys para traer código por SSH
                           (por defecto /etc/status-sv/known_hosts)
`, name, config.DefaultStateFile())
}

// newLogger arma el registro del agent.
func newLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}

	var handler slog.Handler
	if cfg.LogJSON {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	return slog.New(handler).With("componente", "agent")
}
