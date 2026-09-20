package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/JuanBaez77/status-sv/agent/internal/config"
	"github.com/JuanBaez77/status-sv/agent/internal/state"
	"github.com/JuanBaez77/status-sv/agent/internal/ws"
	"github.com/JuanBaez77/status-sv/internal/buildinfo"
)

// runEnroll canjea el token de un solo uso por la credencial propia del agent y
// la guarda en disco. Se corre una vez por servidor, al instalar.
func runEnroll(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("enroll", flag.ContinueOnError)
	serverURL := flags.String("server-url", "", "dirección del control plane (o STATUSSV_SERVER_URL)")
	token := flags.String("token", "", "token de enrolamiento; mejor usar STATUSSV_ENROLL_TOKEN")
	stateFile := flags.String("state-file", "", "dónde guardar la credencial (o STATUSSV_STATE_FILE)")
	hostname := flags.String("hostname", "", "hostname a informar (por defecto, el del sistema)")
	force := flags.Bool("force", false, "vuelve a enrolar aunque ya haya un estado guardado")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if *serverURL != "" {
		cfg.ServerURL = *serverURL
	}
	if *stateFile != "" {
		cfg.StateFile = *stateFile
	}

	// El token por variable de entorno es lo preferido: los argumentos de un
	// proceso los puede leer cualquier usuario del sistema con ps.
	secret := cfg.EnrollToken
	if *token != "" {
		if secret != "" {
			return errors.New("el token vino por STATUSSV_ENROLL_TOKEN y por --token: usá uno solo")
		}
		secret = *token
		fmt.Fprintln(os.Stderr,
			"aviso: pasar el token por --token lo deja visible en ps; STATUSSV_ENROLL_TOKEN es más seguro")
	}

	if cfg.ServerURL == "" {
		return errors.New("falta --server-url (o STATUSSV_SERVER_URL)")
	}
	if secret == "" {
		return errors.New("falta el token de enrolamiento (STATUSSV_ENROLL_TOKEN o --token)")
	}

	if state.Exists(cfg.StateFile) && !*force {
		return fmt.Errorf("ya hay un agent enrolado en %s; usá --force para reemplazarlo", cfg.StateFile)
	}

	reported := *hostname
	if reported == "" {
		reported, err = os.Hostname()
		if err != nil {
			return fmt.Errorf("averiguar el hostname: %w", err)
		}
	}

	log := newLogger(cfg)
	log.InfoContext(ctx, "enrolando el agent",
		"control_plane", cfg.ServerURL,
		"hostname", reported,
	)

	enrolled, err := ws.Enroll(ctx, cfg.ServerURL, secret, reported, buildinfo.Version, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}

	if err := state.Save(cfg.StateFile, enrolled); err != nil {
		return err
	}

	log.InfoContext(ctx, "agent enrolado",
		"servidor", enrolled.ServerName,
		"servidor_id", enrolled.ServerID,
		"estado", cfg.StateFile,
	)

	fmt.Printf(`El agent quedó enrolado como %q.

La credencial está en %s, con permisos 0600. No vuelve a mostrarse ni se puede
recuperar: si se pierde el archivo, hay que enrolar de nuevo con un token nuevo.

Ahora se puede arrancar el servicio:

  agent run

`, enrolled.ServerName, cfg.StateFile)

	return nil
}
