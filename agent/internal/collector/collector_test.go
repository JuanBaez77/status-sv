package collector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/JuanBaez77/status-sv/proto"
)

var fixedNow = time.Date(2026, 9, 21, 12, 0, 0, 123_456_789, time.FixedZone("ART", -3*60*60))

// loadProc arma un /proc en memoria con una carpeta de testdata, para poder
// cambiar archivos entre una lectura y otra.
func loadProc(t *testing.T, fixture string) fstest.MapFS {
	t.Helper()

	proc := fstest.MapFS{}
	root := os.DirFS(path.Join("testdata", fixture))
	err := fs.WalkDir(root, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		proc[name] = &fstest.MapFile{Data: data}
		return nil
	})
	if err != nil {
		t.Fatalf("cargar la muestra %s: %v", fixture, err)
	}
	return proc
}

// fakeDisks reemplaza a statfs y anota sobre qué rutas se llamó.
type fakeDisks struct {
	usage map[string]fsUsage
	fail  map[string]error
	calls []string
}

func (f *fakeDisks) statfs(p string) (fsUsage, error) {
	f.calls = append(f.calls, p)
	if err := f.fail[p]; err != nil {
		return fsUsage{}, err
	}
	usage, ok := f.usage[p]
	if !ok {
		return fsUsage{}, fmt.Errorf("statfs sobre %q, que no es un disco de la muestra", p)
	}
	return usage, nil
}

func vpsDisks() *fakeDisks {
	return &fakeDisks{usage: map[string]fsUsage{
		"/":                   {total: 80 << 30, used: 23 << 30, available: 53 << 30},
		"/boot":               {total: 881 << 20, used: 180 << 20, available: 640 << 20},
		"/boot/efi":           {total: 105 << 20, used: 6 << 20, available: 99 << 20},
		"/mnt/datos respaldo": {total: 500 << 30, used: 350 << 30, available: 150 << 30},
	}}
}

func newTestCollector(proc fs.FS, disks *fakeDisks) *Collector {
	return &Collector{proc: proc, statfs: disks.statfs, now: func() time.Time { return fixedNow }}
}

// advanceStat reemplaza la línea cpu, como si hubieran pasado 10 segundos en
// una máquina de 2 núcleos (2000 ticks): 250 ocupados (12,5%), de los cuales
// 20 robados por el hipervisor (1%), y 10 de espera de disco (0,5%).
func advanceStat(proc fstest.MapFS) {
	stat := string(proc["stat"].Data)
	_, rest, _ := strings.Cut(stat, "\n")
	proc["stat"] = &fstest.MapFile{Data: []byte("cpu  4855 356 5905 1625727 1551 0 502 227 0 0\n" + rest)}
}

func TestCollectFromVPSFixture(t *testing.T) {
	proc := loadProc(t, "vps-ubuntu")
	disks := vpsDisks()
	collector := newTestCollector(proc, disks)
	ctx := context.Background()

	if _, err := collector.Collect(ctx); !errors.Is(err, ErrNoBaseline) {
		t.Fatalf("la primera lectura devolvió %v, se esperaba ErrNoBaseline", err)
	}
	if len(disks.calls) != 0 {
		t.Errorf("la primera lectura no debería medir discos, midió %q", disks.calls)
	}

	advanceStat(proc)
	got, err := collector.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	want := proto.Sample{
		TS:  time.Date(2026, 9, 21, 15, 0, 0, 123_000_000, time.UTC),
		CPU: proto.CPUSample{UsedPercent: 12.5, IOWaitPercent: 0.5, StealPercent: 1, Cores: 2},
		Memory: proto.MemorySample{
			TotalBytes:     8039548 * kib,
			UsedBytes:      (8039548 - 5123456) * kib,
			SwapTotalBytes: 2097148 * kib,
			SwapUsedBytes:  (2097148 - 1835004) * kib,
		},
		Load: proto.LoadSample{Load1: 0.42, Load5: 0.37, Load15: 0.31},
		Disks: []proto.DiskSample{
			{Mountpoint: "/", TotalBytes: 80 << 30, UsedBytes: 23 << 30, AvailableBytes: 53 << 30},
			{Mountpoint: "/boot", TotalBytes: 881 << 20, UsedBytes: 180 << 20, AvailableBytes: 640 << 20},
			{Mountpoint: "/boot/efi", TotalBytes: 105 << 20, UsedBytes: 6 << 20, AvailableBytes: 99 << 20},
			{Mountpoint: "/mnt/datos respaldo", TotalBytes: 500 << 30, UsedBytes: 350 << 30, AvailableBytes: 150 << 30},
		},
		Networks: []proto.NetworkSample{
			{Interface: "ens3", RxBytes: 98123456789, TxBytes: 12345678901},
			{Interface: "wg0", RxBytes: 5242880, TxBytes: 2621440},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("muestra =\n%+v\nse esperaba\n%+v", got, want)
	}

	// Lo que importa tanto como el resultado: nunca se llamó a statfs sobre el
	// NFS, el autofs, los overlay de Docker ni los snaps.
	wantCalls := []string{"/", "/boot", "/boot/efi", "/mnt/datos respaldo"}
	if !slices.Equal(disks.calls, wantCalls) {
		t.Errorf("statfs se llamó sobre %q, se esperaba solo %q", disks.calls, wantCalls)
	}
}

// Un disco que falla no se lleva puesta la muestra.
func TestCollectReturnsPartialSampleWhenADiskFails(t *testing.T) {
	proc := loadProc(t, "vps-ubuntu")
	disks := vpsDisks()
	errBroken := errors.New("error de entrada/salida")
	disks.fail = map[string]error{"/boot": errBroken}
	collector := newTestCollector(proc, disks)

	_, _ = collector.Collect(context.Background())
	advanceStat(proc)
	sample, err := collector.Collect(context.Background())

	var partial *PartialError
	if !errors.As(err, &partial) {
		t.Fatalf("se esperaba *PartialError, llegó %v", err)
	}
	if !errors.Is(err, errBroken) {
		t.Errorf("el error no deja ver la causa: %v", err)
	}
	if got := len(sample.Disks); got != 3 {
		t.Errorf("la muestra trae %d discos, se esperaban los 3 que sí se midieron", got)
	}
	if sample.CPU.UsedPercent != 12.5 || len(sample.Networks) != 2 {
		t.Errorf("la muestra perdió datos que sí se pudieron leer: %+v", sample)
	}
}

func TestCollectReturnsPartialSampleWithoutNetDev(t *testing.T) {
	proc := loadProc(t, "vps-ubuntu")
	delete(proc, "net/dev")
	collector := newTestCollector(proc, vpsDisks())

	_, _ = collector.Collect(context.Background())
	advanceStat(proc)
	sample, err := collector.Collect(context.Background())

	var partial *PartialError
	if !errors.As(err, &partial) {
		t.Fatalf("se esperaba *PartialError, llegó %v", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("el error no deja ver que faltaba el archivo: %v", err)
	}
	if sample.Networks != nil || len(sample.Disks) != 4 {
		t.Errorf("muestra inesperada: %+v", sample)
	}
}

// Sin CPU, memoria o carga no hay muestra que valga la pena mandar.
func TestCollectFailsWithoutCoreFiles(t *testing.T) {
	for _, name := range []string{"stat", "meminfo", "loadavg"} {
		t.Run(name, func(t *testing.T) {
			proc := loadProc(t, "vps-ubuntu")
			collector := newTestCollector(proc, vpsDisks())
			_, _ = collector.Collect(context.Background())

			advanceStat(proc)
			delete(proc, name)
			_, err := collector.Collect(context.Background())

			var partial *PartialError
			if err == nil || errors.As(err, &partial) || errors.Is(err, ErrNoBaseline) {
				t.Fatalf("se esperaba un error que descarte la muestra, llegó %v", err)
			}
		})
	}
}

// Si los contadores retroceden, la lectura se descarta y se vuelve a empezar.
func TestCollectRestartsWhenCountersGoBack(t *testing.T) {
	proc := loadProc(t, "vps-ubuntu")
	collector := newTestCollector(proc, vpsDisks())
	ctx := context.Background()

	advanceStat(proc)
	_, _ = collector.Collect(ctx)

	proc["stat"] = &fstest.MapFile{Data: []byte(readFixture(t, "vps-ubuntu", "stat"))}
	if _, err := collector.Collect(ctx); !errors.Is(err, ErrNoBaseline) {
		t.Fatalf("con contadores hacia atrás llegó %v, se esperaba ErrNoBaseline", err)
	}

	advanceStat(proc)
	if _, err := collector.Collect(ctx); err != nil {
		t.Fatalf("después de volver a empezar: %v", err)
	}
}

func TestCollectHonorsContext(t *testing.T) {
	collector := newTestCollector(loadProc(t, "vps-ubuntu"), vpsDisks())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := collector.Collect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("con el contexto cancelado llegó %v", err)
	}
}

// El contrato limita cuántos discos e interfaces trae una muestra; el colector
// recorta y avisa, en vez de armar una muestra que el control plane rechace.
func TestCollectCapsToContractLimits(t *testing.T) {
	const extra = 6
	proc := loadProc(t, "vps-ubuntu")
	disks := &fakeDisks{usage: map[string]fsUsage{}}

	var mountinfo, netdev strings.Builder
	mountinfo.WriteString("1 0 8:0 / / rw - ext4 /dev/sda rw\n")
	disks.usage["/"] = fsUsage{total: 100, used: 10}
	for i := range proto.MaxDisksPerSample + extra {
		point := fmt.Sprintf("/mnt/d%03d", i)
		fmt.Fprintf(&mountinfo, "%d 1 9:%d / %s rw - ext4 /dev/d%d rw\n", i+2, i, point, i)
		disks.usage[point] = fsUsage{total: 100, used: 10}
	}
	netdev.WriteString("Inter-|\n face |\n")
	for i := range proto.MaxNetworksPerSample + extra {
		fmt.Fprintf(&netdev, "eth%03d: 1 0 0 0 0 0 0 0 2 0 0 0 0 0 0 0\n", i)
	}
	proc["self/mountinfo"] = &fstest.MapFile{Data: []byte(mountinfo.String())}
	proc["net/dev"] = &fstest.MapFile{Data: []byte(netdev.String())}

	collector := newTestCollector(proc, disks)
	_, _ = collector.Collect(context.Background())
	advanceStat(proc)
	sample, err := collector.Collect(context.Background())

	var partial *PartialError
	if !errors.As(err, &partial) || len(partial.Problems) != 2 {
		t.Fatalf("se esperaban dos avisos de recorte, llegó %v", err)
	}
	if len(sample.Disks) != proto.MaxDisksPerSample || len(sample.Networks) != proto.MaxNetworksPerSample {
		t.Errorf("la muestra trae %d discos y %d interfaces", len(sample.Disks), len(sample.Networks))
	}
	if err := sample.Validate(); err != nil {
		t.Errorf("la muestra recortada no cumple el contrato: %v", err)
	}
	if len(disks.calls) != proto.MaxDisksPerSample {
		t.Errorf("se midieron %d discos: los que no entran no deberían medirse", len(disks.calls))
	}
}

func TestCollectSkipsMountpointsTooLongForTheContract(t *testing.T) {
	proc := loadProc(t, "vps-ubuntu")
	long := "/mnt/" + strings.Repeat("x", proto.MaxDeviceNameLen)
	proc["self/mountinfo"] = &fstest.MapFile{Data: []byte(
		"1 0 8:0 / / rw - ext4 /dev/sda rw\n2 1 8:16 / " + long + " rw - ext4 /dev/sdb rw\n")}
	disks := &fakeDisks{usage: map[string]fsUsage{"/": {total: 100, used: 10}}}
	collector := newTestCollector(proc, disks)

	_, _ = collector.Collect(context.Background())
	advanceStat(proc)
	sample, err := collector.Collect(context.Background())

	var partial *PartialError
	if !errors.As(err, &partial) {
		t.Fatalf("se esperaba un aviso, llegó %v", err)
	}
	if len(sample.Disks) != 1 || sample.Disks[0].Mountpoint != "/" {
		t.Errorf("discos = %+v, se esperaba solo la raíz", sample.Disks)
	}
}

// Si ningún montaje parece un disco, conviene que se vea en el registro: lo más
// probable es un sistema de archivos que el colector no conoce.
func TestCollectWarnsWhenNoDiskIsFound(t *testing.T) {
	proc := loadProc(t, "vps-ubuntu")
	proc["self/mountinfo"] = &fstest.MapFile{Data: []byte("1 0 0:30 / / rw - simfs /vz/private/101 rw\n")}
	collector := newTestCollector(proc, &fakeDisks{})

	_, _ = collector.Collect(context.Background())
	advanceStat(proc)
	sample, err := collector.Collect(context.Background())

	var partial *PartialError
	if !errors.As(err, &partial) || !strings.Contains(err.Error(), "ningún montaje") {
		t.Fatalf("se esperaba el aviso de que no hay discos, llegó %v", err)
	}
	if sample.Disks != nil {
		t.Errorf("discos = %+v, se esperaba ninguno", sample.Disks)
	}
}

func TestPartialErrorMessageListsEveryProblem(t *testing.T) {
	err := &PartialError{Problems: []error{errors.New("disco /boot"), errors.New("red")}}
	if got, want := err.Error(), "muestra incompleta: disco /boot; red"; got != want {
		t.Errorf("mensaje = %q, se esperaba %q", got, want)
	}
}

// BenchmarkCollect mide el costo de armar una muestra sin contar la lectura del
// disco: parsear los cinco archivos y elegir los discos. Corre cada 10 segundos,
// así que lo que importa es que sea chico frente a la meta de 1% de CPU.
func BenchmarkCollect(b *testing.B) {
	proc := fstest.MapFS{}
	for _, name := range []string{"stat", "meminfo", "loadavg", "net/dev", "self/mountinfo"} {
		data, err := os.ReadFile(path.Join("testdata", "vps-ubuntu", name))
		if err != nil {
			b.Fatal(err)
		}
		proc[name] = &fstest.MapFile{Data: data}
	}
	collector := newTestCollector(proc, vpsDisks())
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		// Una lectura anterior en cero hace que los contadores siempre avancen.
		collector.prevCPU, collector.hasPrevCPU = cpuTimes{}, true
		if _, err := collector.Collect(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
