//go:build linux

package collector

import (
	"fmt"
	"syscall"
)

// statfs mide un sistema de archivos. Solo se llama sobre los montajes que
// eligió selectDisks, que nunca son de red: sobre un NFS caído, esta llamada
// bloquearía sin plazo.
func statfs(path string) (fsUsage, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return fsUsage{}, fmt.Errorf("statfs %s: %w", path, err)
	}

	// Frsize es la unidad en que vienen contados los bloques. Algunos sistemas
	// de archivos viejos lo dejan en cero; ahí vale Bsize.
	blockSize := stat.Frsize
	if blockSize <= 0 {
		blockSize = stat.Bsize
	}

	usage, err := usageFromBlocks(stat.Blocks, stat.Bfree, blockSize)
	if err != nil {
		return fsUsage{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	return usage, nil
}
