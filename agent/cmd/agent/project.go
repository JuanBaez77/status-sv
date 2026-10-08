package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/JuanBaez77/status-sv/agent/internal/accept"
	"github.com/JuanBaez77/status-sv/agent/internal/config"
	"github.com/JuanBaez77/status-sv/agent/internal/dropin"
	"github.com/JuanBaez77/status-sv/agent/internal/unithelper"
)

// runProject atiende `statussv-agent project ...`: lo que una persona hace
// como root en el servidor para dar de alta un proyecto (ADR-022).
func runProject(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("uso: agent project accept|env PROYECTO")
	}
	switch args[0] {
	case "accept":
		return runProjectAccept(ctx, args[1:])
	case "env":
		return runProjectEnv(ctx, args[1:])
	default:
		return fmt.Errorf("subcomando desconocido: project %q", args[0])
	}
}

// runProjectAccept acepta la propuesta de un proyecto nuevo:
//
//	sudo statussv-agent project accept svdemo
func runProjectAccept(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("project accept", flag.ContinueOnError)
	yes := flags.Bool("yes", false, "acepta sin preguntar (para pruebas)")
	noKey := flags.Bool("no-deploy-key", false, "no genera la deploy key: el repo es público")
	envFile := flags.String("env-file", "", "toma los valores de las variables de este archivo en vez de pedirlos")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("uso: agent project accept [--no-deploy-key] [--env-file ARCHIVO] PROYECTO")
	}
	a, err := newAccepter()
	if err != nil {
		return err
	}
	return a.Accept(ctx, flags.Arg(0), accept.Options{Yes: *yes, NoDeployKey: *noKey, EnvFile: *envFile})
}

// runProjectEnv carga o cambia los valores de las variables requeridas de un
// proyecto declarado:
//
//	sudo statussv-agent project env svdemo
func runProjectEnv(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("project env", flag.ContinueOnError)
	envFile := flags.String("env-file", "", "toma los valores de este archivo en vez de pedirlos")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("uso: agent project env [--env-file ARCHIVO] PROYECTO")
	}
	a, err := newAccepter()
	if err != nil {
		return err
	}
	return a.Env(ctx, flags.Arg(0), *envFile)
}

// newAccepter arma el comando con las rutas del servidor, las mismas que usa
// el instalador.
func newAccepter() (*accept.Accepter, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &accept.Accepter{
		Paths: accept.Paths{
			State:        filepath.Dir(cfg.StateFile),
			Projects:     cfg.ProjectsDir,
			Keys:         cfg.DeployKeysDir,
			Dropin:       dropin.DefaultPath,
			HelperSocket: accept.DefaultHelperSocket,
		},
		Sys: accept.OS{Binary: binary, StateFile: cfg.StateFile, ProjectsDir: cfg.ProjectsDir, Out: os.Stdout},
		In:  os.Stdin,
		Out: os.Stdout,
		// Como el ayudante: la declaración tiene que ser de root, porque
		// de ella sale dónde se escriben los valores.
		LoadSpec: unithelper.LoadTrusted,
	}, nil
}
