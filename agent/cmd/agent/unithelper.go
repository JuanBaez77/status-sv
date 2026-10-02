package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/user"
	"strconv"

	"github.com/JuanBaez77/status-sv/agent/internal/config"
	"github.com/JuanBaez77/status-sv/agent/internal/projectspec"
	"github.com/JuanBaez77/status-sv/agent/internal/unithelper"
)

// runUnitHelper atiende un pedido del agent como root (ADR-019): correr un
// paso declarado de un proyecto systemd o reiniciar sus unidades.
//
// No se corre a mano. Lo arranca systemd, una vez por conexión, desde
// statussv-unit-helper.socket: la conexión llega como la entrada estándar.
func runUnitHelper(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errors.New("unit-helper no lleva argumentos: lo arranca su unidad de socket")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Al diario, por la salida de error: la entrada es la conexión.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("componente", "unit-helper")

	conn, err := net.FileConn(os.Stdin)
	if err != nil {
		return fmt.Errorf("la entrada no es una conexión: unit-helper se arranca desde su unidad de socket: %w", err)
	}
	defer func() { _ = conn.Close() }()
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("la entrada no es un socket unix")
	}

	// El socket ya es solo del grupo del agent; esto además exige que quien
	// se conectó sea el agent (o root, que no necesita al ayudante).
	peer, err := unithelper.PeerUID(unixConn)
	if err != nil {
		return err
	}
	if peer != 0 && !isAgent(peer) {
		log.Warn("pedido de un usuario que no es el agent", "uid", peer)
		unithelper.Reject(unixConn, unithelper.CodeForbidden, "solo el agent puede pedirle al ayudante")
		return nil
	}

	systemd, err := unithelper.ConnectSystemd(ctx)
	if err != nil {
		unithelper.Reject(unixConn, unithelper.CodeInternal, "el ayudante no pudo hablar con systemd")
		return err
	}
	defer systemd.Close()

	helper := &unithelper.Helper{
		ProjectsDir: cfg.ProjectsDir,
		Load:        unithelper.LoadTrusted,
		Systemd:     systemd,
		UserExists: func(name string) bool {
			_, err := user.Lookup(name)
			return err == nil
		},
		Log: log,
	}
	helper.Serve(ctx, unixConn)
	return nil
}

// isAgent dice si uid es el usuario del agent.
func isAgent(uid uint32) bool {
	agent, err := user.Lookup(projectspec.AgentUser)
	if err != nil {
		return false
	}
	return agent.Uid == strconv.FormatUint(uint64(uid), 10)
}
