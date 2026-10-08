//go:build !windows

package accept

import "os"

// fchown cambia el dueño de un archivo abierto.
func fchown(f *os.File, uid, gid int) error {
	return f.Chown(uid, gid)
}
