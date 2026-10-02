//go:build !linux

package unithelper

import (
	"errors"
	"net"
)

// PeerUID solo existe en Linux, que es donde corre el ayudante.
func PeerUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("las credenciales del socket solo se leen en Linux")
}
