package collector

import (
	"math"
	"slices"
	"testing"
)

func points(mounts []mount) []string {
	result := make([]string, len(mounts))
	for i, m := range mounts {
		result[i] = m.point
	}
	return result
}

func TestSelectDisksFromFixtures(t *testing.T) {
	tests := []struct {
		fixture string
		want    []string
	}{
		// Quedan afuera los pseudo sistemas de archivos, los overlay de Docker,
		// los snaps, el NFS y el autofs. /tmp, /var/tmp y /var/lib/status-sv
		// son bind mounts del disco raíz y no se repiten.
		{fixture: "vps-ubuntu", want: []string{"/", "/boot", "/boot/efi", "/mnt/datos respaldo"}},

		// Captura real bajo la unidad del agent. La raíz es overlay porque
		// corrió en un contenedor; /home y /home/backups tenían discos
		// montados, y con ProtectHome=true no aparecen.
		{fixture: "systemd-sandbox", want: []string{"/etc/hosts", "/mnt/datos respaldo"}},

		// En un contenedor, lo único en disco son los archivos que Docker
		// monta desde el host, y los tres son del mismo disco.
		{fixture: "docker-desktop", want: []string{"/etc/hosts"}},
	}

	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			mounts, err := parseMountinfo(readFixture(t, tt.fixture, "self", "mountinfo"))
			if err != nil {
				t.Fatalf("parseMountinfo: %v", err)
			}
			if got := points(selectDisks(mounts)); !slices.Equal(got, tt.want) {
				t.Errorf("discos = %q, se esperaba %q", got, tt.want)
			}
		})
	}
}

// Un disco tapado por otro montaje no se informa: statfs mediría al de arriba.
func TestSelectDisksSkipsHiddenMounts(t *testing.T) {
	const raw = `1 0 8:1 / / rw - ext4 /dev/sda1 rw
2 1 8:17 / /srv/datos rw - ext4 /dev/sdb1 rw
3 2 8:33 / /srv/datos rw - xfs /dev/sdc1 rw
4 2 8:49 / /srv/datos/viejo rw - ext4 /dev/sdd1 rw
5 1 8:65 / /home/juan rw - ext4 /dev/sde1 rw
6 1 0:40 / /home rw - tmpfs tmpfs rw
`
	// 2 está tapado por 3, que se montó encima en la misma ruta. 4 colgaba de 2
	// y quedó debajo de 3. 5 quedó debajo del tmpfs que se montó en /home.
	mounts, err := parseMountinfo(raw)
	if err != nil {
		t.Fatalf("parseMountinfo: %v", err)
	}

	got := selectDisks(mounts)
	if want := []string{"/", "/srv/datos"}; !slices.Equal(points(got), want) {
		t.Fatalf("discos = %q, se esperaba %q", points(got), want)
	}
	if got[1].device != "8:33" {
		t.Errorf("/srv/datos es el dispositivo %s, se esperaba el de arriba (8:33)", got[1].device)
	}
}

// El resultado no puede depender del orden de las líneas.
func TestSelectDisksIsDeterministic(t *testing.T) {
	const raw = `1 0 8:1 / / rw - ext4 /dev/sda1 rw
2 1 8:17 /a /srv/bbb rw - ext4 /dev/sdb1 rw
3 1 8:17 /b /srv/aaa rw - ext4 /dev/sdb1 rw
`
	mounts, err := parseMountinfo(raw)
	if err != nil {
		t.Fatalf("parseMountinfo: %v", err)
	}

	want := []string{"/", "/srv/aaa"}
	for range 3 {
		if got := points(selectDisks(mounts)); !slices.Equal(got, want) {
			t.Fatalf("discos = %q, se esperaba %q", got, want)
		}
		slices.Reverse(mounts)
	}
}

// Una tabla corrupta con un ciclo de padres no puede colgar al agent.
func TestSelectDisksSurvivesParentCycles(t *testing.T) {
	const raw = `1 2 8:1 / / rw - ext4 /dev/sda1 rw
2 1 8:17 / /srv rw - ext4 /dev/sdb1 rw
`
	mounts, err := parseMountinfo(raw)
	if err != nil {
		t.Fatalf("parseMountinfo: %v", err)
	}
	_ = selectDisks(mounts)
}

func TestParseMountinfoRejectsBrokenLines(t *testing.T) {
	tests := map[string]string{
		"pocos campos":   "36 35 98:0 / /mnt rw\n",
		"sin separador":  "36 35 98:0 / /mnt rw master:1 ext3 /dev/root rw\n",
		"sin tipo":       "36 35 98:0 / /mnt rw a b -\n",
		"ID no numérico": "x 35 98:0 / /mnt rw - ext3 /dev/root rw\n",
		"padre roto":     "36 y 98:0 / /mnt rw - ext3 /dev/root rw\n",
	}

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseMountinfo(raw); err == nil {
				t.Fatal("se esperaba un error")
			}
		})
	}
}

// Los campos opcionales pueden ser cero o varios antes del guion.
func TestParseMountinfoOptionalFields(t *testing.T) {
	const raw = `36 35 98:0 / /sin-opcionales rw - ext4 /dev/a rw
37 35 98:1 / /con-varios rw shared:1 master:2 propagate_from:3 - xfs /dev/b rw
`
	mounts, err := parseMountinfo(raw)
	if err != nil {
		t.Fatalf("parseMountinfo: %v", err)
	}
	if mounts[0].fsType != "ext4" || mounts[1].fsType != "xfs" {
		t.Errorf("tipos = %q y %q, se esperaba ext4 y xfs", mounts[0].fsType, mounts[1].fsType)
	}
}

func TestUnescapeMount(t *testing.T) {
	tests := map[string]string{
		`/mnt/datos\040respaldo`:  "/mnt/datos respaldo",
		`/mnt/con\011tab`:         "/mnt/con\ttab",
		`/mnt/con\012salto`:       "/mnt/con\nsalto",
		`/mnt/barra\134invertida`: `/mnt/barra\invertida`,
		`/mnt/sin-escapes`:        "/mnt/sin-escapes",
		`/mnt/no-octal\999`:       `/mnt/no-octal\999`,
		`/mnt/corto\04`:           `/mnt/corto\04`,
		`/mnt/fuera-de-rango\777`: `/mnt/fuera-de-rango\777`,
	}

	for raw, want := range tests {
		if got := unescapeMount(raw); got != want {
			t.Errorf("unescapeMount(%q) = %q, se esperaba %q", raw, got, want)
		}
	}
}

func TestIsDiskFilesystem(t *testing.T) {
	tests := map[string]bool{
		"ext4": true, "xfs": true, "btrfs": true, "zfs": true, "vfat": true,
		"nfs": false, "nfs4": false, "cifs": false, "fuse.sshfs": false,
		"overlay": false, "tmpfs": false, "squashfs": false, "autofs": false, "proc": false,
	}

	for fsType, want := range tests {
		if got := isDiskFilesystem(fsType); got != want {
			t.Errorf("isDiskFilesystem(%q) = %v, se esperaba %v", fsType, got, want)
		}
	}
}

func TestUsageFromBlocks(t *testing.T) {
	got, err := usageFromBlocks(1000, 250, int64(4096))
	if err != nil {
		t.Fatalf("usageFromBlocks: %v", err)
	}
	if want := (fsUsage{total: 1000 * 4096, used: 750 * 4096}); got != want {
		t.Errorf("uso = %+v, se esperaba %+v", got, want)
	}

	// En 32 bits el tamaño de bloque es int32.
	if _, err := usageFromBlocks(1000, 250, int32(4096)); err != nil {
		t.Errorf("con int32: %v", err)
	}
}

func TestUsageFromBlocksRejectsImpossibleValues(t *testing.T) {
	tests := []struct {
		name         string
		blocks, free uint64
		blockSize    int64
	}{
		{name: "bloque de tamaño cero", blocks: 10, free: 5, blockSize: 0},
		{name: "bloque negativo", blocks: 10, free: 5, blockSize: -4096},
		{name: "más libres que total", blocks: 10, free: 11, blockSize: 4096},
		{name: "total que no entra en 64 bits", blocks: math.MaxUint64 / 2, free: 0, blockSize: 4096},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := usageFromBlocks(tt.blocks, tt.free, tt.blockSize); err == nil {
				t.Fatal("se esperaba un error")
			}
		})
	}
}
