package collector

import (
	"errors"
	"fmt"
	"maps"
	"math/bits"
	"slices"
	"strconv"
	"strings"
)

// mount es una línea de /proc/self/mountinfo, con lo que hace falta para
// decidir si es un disco que vale la pena informar.
type mount struct {
	id     int
	parent int
	// device es "mayor:menor" e identifica al sistema de archivos: los bind
	// mounts de un mismo disco comparten el valor.
	device string
	// point es dónde está montado, visto desde el agent.
	point  string
	fsType string
}

// fsUsage es el espacio de un sistema de archivos, en bytes.
type fsUsage struct {
	total     uint64
	used      uint64
	available uint64
}

// parseMountinfo lee /proc/self/mountinfo. El formato está en proc(5):
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
//	(1)(2) (3)  (4)   (5)      (6)       (7)   (8) (9)    (10)        (11)
//
// Los campos opcionales (7) pueden ser cero o más, y terminan en un guion.
//
// Se lee la tabla del propio proceso y no la de /proc/1: bajo systemd el agent
// corre en su propio espacio de montajes, y lo que importa es qué puede medir
// él, no lo que ve el sistema.
func parseMountinfo(raw string) ([]mount, error) {
	var mounts []mount
	for line := range strings.Lines(raw) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m, err := parseMountLine(line)
		if err != nil {
			return nil, fmt.Errorf("línea de mountinfo %q: %w", line, err)
		}
		mounts = append(mounts, m)
	}
	return mounts, nil
}

func parseMountLine(line string) (mount, error) {
	const fixedFields = 6

	fields := strings.Fields(line)
	if len(fields) < fixedFields+3 {
		return mount{}, fmt.Errorf("trae %d campos y hacen falta al menos %d", len(fields), fixedFields+3)
	}

	separator := slices.Index(fields[fixedFields:], "-")
	if separator < 0 {
		return mount{}, errors.New("falta el guion que separa los campos opcionales")
	}
	fsTypeIndex := fixedFields + separator + 1
	if fsTypeIndex >= len(fields) {
		return mount{}, errors.New("falta el tipo de sistema de archivos")
	}

	id, err := strconv.Atoi(fields[0])
	if err != nil {
		return mount{}, fmt.Errorf("ID de montaje: %w", err)
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return mount{}, fmt.Errorf("ID del montaje padre: %w", err)
	}

	return mount{
		id:     id,
		parent: parent,
		device: fields[2],
		point:  unescapeMount(fields[4]),
		fsType: fields[fsTypeIndex],
	}, nil
}

// unescapeMount deshace el escape del kernel: en mountinfo, los espacios,
// tabuladores, saltos de línea y barras invertidas de una ruta vienen como
// \040, \011, \012 y \134.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if value, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(value))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// selectDisks elige de la tabla de montajes los discos que se informan: los
// sistemas de archivos en disco local, visibles de verdad desde el agent, una
// sola vez por dispositivo y ordenados por punto de montaje.
func selectDisks(mounts []mount) []mount {
	byID := make(map[int]mount, len(mounts))
	for _, m := range mounts {
		byID[m.id] = m
	}

	// Un mismo disco aparece varias veces cuando hay bind mounts: con
	// PrivateTmp=, systemd monta en /tmp un directorio de la raíz. Se informa
	// una sola vez, con el punto de montaje más corto, que suele ser la raíz
	// del sistema de archivos.
	chosen := make(map[string]mount)
	for _, m := range mounts {
		if !isDiskFilesystem(m.fsType) || !visible(m, byID, mounts) {
			continue
		}
		if current, ok := chosen[m.device]; ok && !shorterPoint(m, current) {
			continue
		}
		chosen[m.device] = m
	}

	disks := slices.Collect(maps.Values(chosen))
	slices.SortFunc(disks, func(a, b mount) int { return strings.Compare(a.point, b.point) })
	return disks
}

// isDiskFilesystem dice si el tipo corresponde a un disco local.
//
// Es una lista de permitidos y no de prohibidos, a propósito: statfs sobre un
// NFS o un sshfs que no responde bloquea al proceso sin plazo, y un sistema de
// archivos desconocido no vale ese riesgo. Quedan afuera también los que no son
// discos (tmpfs, proc, cgroup), los de Docker (overlay) y las imágenes de snap
// (squashfs), que siempre están llenas.
func isDiskFilesystem(fsType string) bool {
	switch fsType {
	case "ext2", "ext3", "ext4", "xfs", "btrfs", "zfs", "f2fs", "bcachefs",
		"jfs", "reiserfs", "vfat", "exfat", "ntfs", "ntfs3":
		return true
	}
	return false
}

// visible dice si el punto de montaje de m muestra de verdad a m.
//
// Un montaje puede estar en la tabla y aun así quedar tapado por otro que se
// montó después en la misma ruta o en una carpeta que la contiene. Un statfs
// sobre esa ruta mediría al de arriba, y el panel mostraría sus números con la
// etiqueta del disco tapado: un dato equivocado que nadie notaría.
//
// Con la unidad del agent, systemd 255 no produce este caso: ProtectHome= saca
// de la tabla el /home real en vez de dejarlo tapado (está verificado, ver
// testdata/systemd-sandbox). El chequeo queda para otras versiones y para
// montajes apilados a mano.
//
// m queda tapado si otro montaje cuelga del mismo árbol que m, en su misma
// ruta o en una que la contiene, sin ser uno de sus ancestros. Ese montaje se
// hizo después que m (si no, m colgaría de él) y por eso lo tapa. Se decide por
// la estructura de la tabla, no por el orden de las líneas.
func visible(m mount, byID map[int]mount, all []mount) bool {
	// La cadena de m: el propio montaje y todos aquellos de los que cuelga.
	chain := map[int]bool{m.id: true}
	for current := m; ; {
		parent, ok := byID[current.parent]
		if !ok || chain[parent.id] {
			break
		}
		chain[parent.id] = true
		current = parent
	}

	for _, other := range all {
		if chain[other.id] || !chain[other.parent] {
			continue
		}
		if containsPath(other.point, m.point) {
			return false
		}
	}
	return true
}

// containsPath dice si la ruta outer es inner o una de sus carpetas superiores.
func containsPath(outer, inner string) bool {
	return outer == inner || outer == "/" || strings.HasPrefix(inner, outer+"/")
}

// shorterPoint decide entre dos montajes del mismo dispositivo: gana el punto
// de montaje más corto y, a igual largo, el primero en orden alfabético, para
// que el resultado no dependa del orden de la tabla.
func shorterPoint(a, b mount) bool {
	if len(a.point) != len(b.point) {
		return len(a.point) < len(b.point)
	}
	return a.point < b.point
}

// usageFromBlocks pasa a bytes lo que devuelve statfs. Lo usado y lo
// disponible coinciden con las columnas Used y Avail de df: lo usado es todo lo
// que no está libre, y lo disponible es lo libre menos la reserva para root.
//
// Es genérica porque el tamaño de bloque es int64 en amd64 y arm64 pero int32
// en las arquitecturas de 32 bits.
func usageFromBlocks[T int32 | int64](blocks, free, available uint64, blockSize T) (fsUsage, error) {
	if blockSize <= 0 {
		return fsUsage{}, fmt.Errorf("tamaño de bloque imposible: %d", blockSize)
	}
	if free > blocks {
		return fsUsage{}, fmt.Errorf("hay más bloques libres (%d) que bloques en total (%d)", free, blocks)
	}

	size := uint64(blockSize)
	overflow, total := bits.Mul64(blocks, size)
	if overflow != 0 {
		return fsUsage{}, fmt.Errorf("el tamaño no entra en 64 bits: %d bloques de %d bytes", blocks, size)
	}

	// Lo usado y lo disponible son a lo sumo el total, que ya se sabe que
	// entra. Lo disponible se acota a lo libre: un sistema de archivos que
	// informara más no puede romper el contrato.
	return fsUsage{
		total:     total,
		used:      (blocks - free) * size,
		available: min(available, free) * size,
	}, nil
}
