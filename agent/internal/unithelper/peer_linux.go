package unithelper

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// PeerUID devuelve el usuario del proceso que está del otro lado de un socket
// unix, según el kernel (SO_PEERCRED): no es algo que el otro lado pueda
// declarar a su gusto.
func PeerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var (
		cred    *unix.Ucred
		credErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) //nolint:gosec // un descriptor de archivo siempre entra en int
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, fmt.Errorf("leer las credenciales del socket: %w", credErr)
	}
	return cred.Uid, nil
}
