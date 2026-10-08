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
)

// runProject atiende `statussv-agent project ...`: lo que una persona hace
// como root en el servidor para dar de alta un proyecto (ADR-022).
func runProject(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("uso: agent project accept PROYECTO")
	}
	switch args[0] {
	case "accept":
		return runProjectAccept(ctx, args[1:])
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
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("uso: agent project accept [--no-deploy-key] PROYECTO")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	a := &accept.Accepter{
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
	}
	return a.Accept(ctx, flags.Arg(0), accept.Options{Yes: *yes, NoDeployKey: *noKey})
}
