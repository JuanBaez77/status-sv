package procs

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/JuanBaez77/status-sv/proto"
)

const btime = 1790563064

// statLine arma un /proc/<pid>/stat con los campos que importan; el resto va
// en cero. El de cron es real, capturado de WSL Ubuntu.
func statLine(pid int, comm string, flags, utime, stime, start, rss uint64) string {
	return fmt.Sprintf("%d (%s) S 1 %d %d 0 -1 %d 0 0 0 0 %d %d 0 0 20 0 1 0 %d 1000 %d 0 0\n",
		pid, comm, pid, pid, flags, utime, stime, start, rss)
}

const cronStat = "176 (cron) S 1 176 176 0 -1 4194560 620 0 3 0 0 1 0 0 20 0 1 0 408 4341760 640 18446744073709551615 93980621352960 93980621383637 140721264984864 0 0 0 0 0 65537 1 0 0 17 0 0 0 0 0 0 93980621396272 93980621398344 93981202288640 140721264991905 140721264991926 140721264991926 140721264992233 0\n"

var containerID = strings.Repeat("ab", 32)

// fakeProc es un /proc con un proceso de cada clase.
func fakeProc() fstest.MapFS {
	return fstest.MapFS{
		"stat": {Data: []byte(fmt.Sprintf("cpu  1 2 3 4\nbtime %d\nprocesses 900\n", btime))},

		// Un servicio: se saltea sin leer más que su cgroup.
		"176/cgroup":  {Data: []byte("0::/system.slice/cron.service\n")},
		"176/stat":    {Data: []byte(cronStat)},
		"176/status":  {Data: []byte("Name:\tcron\nUid:\t0\t0\t0\t0\n")},
		"176/cmdline": {Data: []byte("/usr/sbin/cron\x00-f\x00-P\x00")},

		// Un contenedor.
		"500/cgroup": {Data: []byte("0::/system.slice/docker-" + containerID + ".scope\n")},

		// Lo arrancado a mano en una sesión SSH, con una contraseña en los
		// argumentos que no puede salir.
		"900/cgroup":  {Data: []byte("0::/user.slice/user-1000.slice/session-3.scope\n")},
		"900/stat":    {Data: []byte(statLine(900, "python3", 4194560, 300, 100, 5000, 25600))},
		"900/status":  {Data: []byte("Name:\tpython3\nUid:\t1000\t1000\t1000\t1000\n")},
		"900/cmdline": {Data: []byte("python3\x00importar.py\x00--db-password=Hunter2\x00--lote\x00500\x00")},

		// Un nombre con paréntesis y espacios, de un usuario sin nombre.
		"901/cgroup":  {Data: []byte("0::/user.slice/user-1001.slice/session-4.scope\n")},
		"901/stat":    {Data: []byte(statLine(901, "raro) (nombre", 4194560, 0, 0, 6000, 100))},
		"901/status":  {Data: []byte("Uid:\t4242\t4242\t4242\t4242\n")},
		"901/cmdline": {Data: []byte("")},

		// Un hilo del kernel en un cgroup que parece de usuario: igual se saltea.
		"902/cgroup":  {Data: []byte("0::/user.slice/user-1000.slice/session-3.scope\n")},
		"902/stat":    {Data: []byte(statLine(902, "kworker/0:1", 0x208040, 0, 0, 10, 0))},
		"902/status":  {Data: []byte("Uid:\t0\t0\t0\t0\n")},
		"902/cmdline": {Data: []byte("")},

		// Uno que terminó entre la lectura del cgroup y la del resto.
		"903/cgroup": {Data: []byte("0::/user.slice/user-1000.slice/session-3.scope\n")},

		// Lo que no es un proceso.
		"self/cgroup": {Data: []byte("0::/user.slice\n")},
		"meminfo":     {Data: []byte("MemTotal: 1 kB\n")},
	}
}

func fakeEtc() fstest.MapFS {
	return fstest.MapFS{
		"passwd": {Data: []byte("root:x:0:0:root:/root:/bin/bash\njuan:x:1000:1000:Juan:/home/juan:/bin/bash\n")},
	}
}

func newTestScanner(proc fstest.MapFS) (*Scanner, *time.Time) {
	now := time.Unix(btime+3600, 0)
	s := NewScanner(proc, fakeEtc(), 4, 4096)
	s.now = func() time.Time { return now }
	return s, &now
}

func TestSnapshotFindsOnlyUnmanaged(t *testing.T) {
	s, _ := newTestScanner(fakeProc())
	processes, omitted, src := s.Snapshot(context.Background())
	if src.Status != proto.SourceOK {
		t.Fatalf("fuente: %+v", src)
	}
	if omitted != 0 {
		t.Errorf("omitidos: %d", omitted)
	}
	if len(processes) != 2 {
		t.Fatalf("esperaba 2 procesos no gestionados, hay %d: %+v", len(processes), processes)
	}

	// Ordenados por memoria: python3 ocupa 100 MiB.
	py := processes[0]
	want := proto.Process{
		PID:       900,
		Name:      "python3",
		Command:   "python3 importar.py --db-password=*** --lote 500",
		User:      "juan",
		StartedAt: time.Unix(btime+50, 0).UTC(),
		Usage:     proto.Usage{CPUPercent: 0, MemoryBytes: 25600 * 4096},
	}
	if fmt.Sprintf("%+v", py) != fmt.Sprintf("%+v", want) {
		t.Errorf("python3:\n obtuve   %+v\n esperaba %+v", py, want)
	}

	raro := processes[1]
	if raro.Name != "raro) (nombre" || raro.User != "4242" || raro.Command != "" {
		t.Errorf("proceso con nombre raro mal leído: %+v", raro)
	}

	for _, p := range processes {
		if err := p.Validate(); err != nil {
			t.Errorf("el proceso %d no pasa el contrato: %v", p.PID, err)
		}
	}
}

func TestCPUBetweenSnapshots(t *testing.T) {
	proc := fakeProc()
	s, now := newTestScanner(proc)
	s.Snapshot(context.Background())

	// 10 s después, python3 sumó 10 s de CPU (1000 ticks): un núcleo entero de cuatro.
	*now = now.Add(10 * time.Second)
	proc["900/stat"] = &fstest.MapFile{Data: []byte(statLine(900, "python3", 4194560, 1300, 100, 5000, 25600))}
	processes, _, _ := s.Snapshot(context.Background())
	if got := processes[0].Usage.CPUPercent; got < 24.99 || got > 25.01 {
		t.Errorf("CPU de python3: obtuve %v, esperaba 25", got)
	}
}

// Si el PID se reusa, es otro proceso: no se compara con el anterior.
func TestReusedPIDStartsOver(t *testing.T) {
	proc := fakeProc()
	s, now := newTestScanner(proc)
	s.Snapshot(context.Background())

	*now = now.Add(10 * time.Second)
	proc["900/stat"] = &fstest.MapFile{Data: []byte(statLine(900, "python3", 4194560, 9000, 100, 7000, 25600))}
	processes, _, _ := s.Snapshot(context.Background())
	if got := processes[0].Usage.CPUPercent; got != 0 {
		t.Errorf("un PID reusado no tiene porcentaje todavía, obtuve %v", got)
	}
}

func TestWithoutCgroupV2(t *testing.T) {
	proc := fstest.MapFS{
		"stat":        {Data: []byte(fmt.Sprintf("btime %d\n", btime))},
		"900/cgroup":  {Data: []byte("12:memory:/user.slice\n1:name=systemd:/user.slice/session-3.scope\n")},
		"900/stat":    {Data: []byte(statLine(900, "bash", 0, 0, 0, 1, 1))},
		"900/status":  {Data: []byte("Uid:\t0\t0\t0\t0\n")},
		"900/cmdline": {Data: []byte("bash\x00")},
	}
	s, _ := newTestScanner(proc)
	processes, _, src := s.Snapshot(context.Background())
	if src.Status != proto.SourceUnavailable || processes != nil {
		t.Errorf("sin cgroup v2 la fuente no está disponible: %+v, %d procesos", src, len(processes))
	}
	if err := src.Validate(); err != nil {
		t.Errorf("la fuente no pasa el contrato: %v", err)
	}
}

// Con más procesos que el tope, entran los que más CPU usan aunque ocupen
// poca memoria, y el resto se completa por memoria.
func TestChooseKeepsCPUHogsAndBigOnes(t *testing.T) {
	var all []candidate
	for i := range 30 {
		all = append(all, candidate{Process: proto.Process{
			PID:   1000 + i,
			Usage: proto.Usage{MemoryBytes: uint64(1000+i) << 20},
		}})
	}
	// Chiquito en memoria, pero se come un núcleo.
	all = append(all, candidate{Process: proto.Process{
		PID:   42,
		Usage: proto.Usage{CPUPercent: 25, MemoryBytes: 1 << 20},
	}})

	chosen := choose(all)
	if len(chosen) != proto.MaxProcesses {
		t.Fatalf("elegidos: %d", len(chosen))
	}
	pids := make(map[int]bool)
	for _, c := range chosen {
		pids[c.PID] = true
	}
	if !pids[42] {
		t.Error("el que se come un núcleo tiene que estar")
	}
	if !pids[1029] || pids[1000] {
		t.Error("el resto se elige por memoria: tiene que estar el más grande y no el más chico")
	}
	for i := 1; i < len(chosen); i++ {
		if chosen[i].Usage.MemoryBytes > chosen[i-1].Usage.MemoryBytes {
			t.Fatal("la lista tiene que quedar ordenada por memoria")
		}
	}
}

func TestSnapshotReportsOmitted(t *testing.T) {
	proc := fakeProc()
	for i := range 25 {
		pid := 2000 + i
		dir := fmt.Sprint(pid)
		proc[dir+"/cgroup"] = &fstest.MapFile{Data: []byte("0::/user.slice/user-1000.slice/session-9.scope\n")}
		proc[dir+"/stat"] = &fstest.MapFile{Data: []byte(statLine(pid, "sleep", 0, 0, 0, 100, 10))}
		proc[dir+"/status"] = &fstest.MapFile{Data: []byte("Uid:\t1000\t1000\t1000\t1000\n")}
		proc[dir+"/cmdline"] = &fstest.MapFile{Data: []byte("sleep\x00infinity\x00")}
	}
	s, _ := newTestScanner(proc)
	processes, omitted, _ := s.Snapshot(context.Background())
	if len(processes) != proto.MaxProcesses || omitted != 27-proto.MaxProcesses {
		t.Errorf("con 27 procesos esperaba %d y %d omitidos; hay %d y %d",
			proto.MaxProcesses, 27-proto.MaxProcesses, len(processes), omitted)
	}
}

func TestCommandIsMaskedBeforeTruncating(t *testing.T) {
	// El secreto cae justo sobre el borde del recorte: si se recortara primero,
	// quedaría la mitad a la vista.
	long := strings.Repeat("x", proto.MaxCommandLen-20)
	got := command([]byte("app\x00" + long + "\x00--token=SECRETOSECRETOSECRETO\x00"))
	if strings.Contains(got, "SECRETO") {
		t.Fatalf("quedó parte del secreto: %q", got)
	}
	if len(got) > proto.MaxCommandLen {
		t.Errorf("la línea ocupa %d bytes", len(got))
	}
}

func TestCleanKeepsUTF8(t *testing.T) {
	s := strings.Repeat("ñ", 40) // 80 bytes
	got := clean(s, 63)
	if len(got) > 63 || !strings.HasPrefix(s, got) {
		t.Errorf("recorte inválido: %q (%d bytes)", got, len(got))
	}
	if got := clean("a\x1b[31mb\nc", 64); got != "a [31mb c" {
		t.Errorf("caracteres de control: %q", got)
	}
}

// Contra el /proc de verdad, en Linux. En WSL con systemd encuentra lo
// arrancado en la sesión; en el runner de CI puede no encontrar nada, pero
// todo lo que devuelva tiene que pasar el contrato.
func TestRealProc(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc es de Linux")
	}
	s := NewScanner(os.DirFS("/proc"), os.DirFS("/etc"), runtime.NumCPU(), os.Getpagesize())
	processes, omitted, src := s.Snapshot(context.Background())
	if src.Status == proto.SourceError {
		t.Fatalf("fuente: %+v", src)
	}
	for _, p := range processes {
		if err := p.Validate(); err != nil {
			t.Errorf("el proceso %d no pasa el contrato: %v", p.PID, err)
		}
	}
	t.Logf("fuente %s: %d procesos no gestionados, %d omitidos", src.Status, len(processes), omitted)
	for _, p := range processes {
		t.Logf("  %d %s (%s) %.1f%% %d MiB: %s", p.PID, p.Name, p.User, p.Usage.CPUPercent, p.Usage.MemoryBytes>>20, p.Command)
	}
}
