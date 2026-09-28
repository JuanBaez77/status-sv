// Package procs encuentra los procesos que no pertenecen a ningún contenedor
// ni servicio del sistema: lo que alguien arrancó a mano en el servidor. El
// panel los muestra en solo lectura.
//
// Recorre /proc una vez por foto. Lo primero que lee de cada proceso es su
// cgroup, que ya dice si es de un contenedor o de un servicio: solo de los que
// quedan se leen el resto de los archivos, así que un servidor con cientos de
// procesos gestionados cuesta una lectura chica por proceso.
package procs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/JuanBaez77/status-sv/agent/internal/cgroups"
	"github.com/JuanBaez77/status-sv/agent/internal/redact"
	"github.com/JuanBaez77/status-sv/agent/internal/usage"
	"github.com/JuanBaez77/status-sv/proto"
)

const (
	// clockTick es USER_HZ, la unidad de los tiempos de /proc/<pid>/stat. Es
	// 100 en todo Linux de x86 y ARM desde hace décadas; leerlo con sysconf
	// exigiría cgo.
	clockTick = 10 * time.Millisecond
	// pfKthread es la marca de los hilos del kernel en el campo flags.
	pfKthread = 0x00200000
	// busiestByCPU es cuántos lugares de la lista se reservan para los que más
	// CPU usan; el resto se llena por memoria. Así aparece tanto un proceso
	// chico que se come un núcleo como uno quieto que ocupa gigas.
	busiestByCPU = proto.MaxProcesses / 2
	// minCPUPercent es lo mínimo para entrar por CPU: por debajo es ruido.
	minCPUPercent = 0.1
)

// Scanner recorre /proc. Recuerda el CPU de cada proceso entre fotos, así que
// no es seguro usarlo desde varias goroutines.
type Scanner struct {
	proc     fs.FS
	etc      fs.FS
	tracker  *usage.Tracker
	pageSize uint64
	now      func() time.Time
}

// NewScanner arma un Scanner sobre /proc y /etc (de /etc solo lee passwd, para
// los nombres de usuario). cores es la cantidad de núcleos de la máquina.
func NewScanner(proc, etc fs.FS, cores, pageSize int) *Scanner {
	return &Scanner{
		proc:     proc,
		etc:      etc,
		tracker:  usage.NewTracker(cores),
		pageSize: uint64(max(pageSize, 1)), //nolint:gosec // el tamaño de página es positivo
		now:      time.Now,
	}
}

// candidate es un proceso no gestionado antes de elegir cuáles se muestran.
type candidate struct {
	proto.Process
	uid string
}

// Snapshot devuelve los procesos no gestionados más pesados, cuántos quedaron
// afuera por el tope y el estado de la fuente.
func (s *Scanner) Snapshot(ctx context.Context) ([]proto.Process, int, proto.RuntimeSource) {
	entries, err := fs.ReadDir(s.proc, ".")
	if err != nil {
		return nil, 0, proto.SourceFailed(fmt.Errorf("leer /proc: %w", err))
	}
	boot, err := s.bootTime()
	if err != nil {
		return nil, 0, proto.SourceFailed(err)
	}

	now := s.now()
	var candidates []candidate
	classified := 0
	for _, entry := range entries {
		if ctx.Err() != nil {
			return nil, 0, proto.SourceFailed(ctx.Err())
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}

		// Un proceso puede terminar en cualquier momento del recorrido: si
		// falta un archivo, se lo saltea.
		raw, err := fs.ReadFile(s.proc, entry.Name()+"/cgroup")
		if err != nil {
			continue
		}
		group, ok := cgroups.ParseProcCgroup(string(raw))
		if !ok {
			continue
		}
		classified++
		if cgroups.Classify(group).Kind != cgroups.KindUnmanaged {
			continue
		}

		c, ok := s.read(pid, boot, now)
		if ok {
			candidates = append(candidates, c)
		}
	}
	s.tracker.Sweep()

	// Sin cgroup v2 no se puede saber de quién es cada proceso, y mostrar
	// todos como "a mano" sería mentir.
	if classified == 0 {
		return nil, 0, proto.RuntimeSource{
			Status: proto.SourceUnavailable,
			Detail: "sin cgroup v2 no se puede saber qué procesos no son de un contenedor ni de un servicio",
		}
	}

	users := s.users()
	chosen := choose(candidates)
	processes := make([]proto.Process, len(chosen))
	for i, c := range chosen {
		c.User = c.uid
		if name, ok := users[c.uid]; ok {
			c.User = name
		}
		processes[i] = c.Process
	}
	return processes, len(candidates) - len(chosen), proto.RuntimeSource{Status: proto.SourceOK}
}

// read arma un proceso con sus archivos de /proc. Devuelve false si no es un
// proceso de usuario (un hilo del kernel) o si terminó mientras se lo leía.
func (s *Scanner) read(pid int, boot, now time.Time) (candidate, bool) {
	dir := strconv.Itoa(pid)
	statRaw, err := fs.ReadFile(s.proc, dir+"/stat")
	if err != nil {
		return candidate{}, false
	}
	st, err := parseStat(string(statRaw))
	if err != nil || st.flags&pfKthread != 0 {
		return candidate{}, false
	}
	statusRaw, err := fs.ReadFile(s.proc, dir+"/status")
	if err != nil {
		return candidate{}, false
	}
	uid, ok := parseUID(string(statusRaw))
	if !ok {
		return candidate{}, false
	}
	cmdline, err := fs.ReadFile(s.proc, dir+"/cmdline")
	if err != nil {
		return candidate{}, false
	}

	started := boot.Add(time.Duration(st.startTicks) * clockTick) //nolint:gosec // ticks desde el arranque entran en int64
	cpu := time.Duration(st.utime+st.stime) * clockTick           //nolint:gosec // idem
	// La clave incluye el arranque: si el PID se reusa, es otro proceso y no
	// se compara con el anterior.
	key := dir + "@" + strconv.FormatUint(st.startTicks, 10)
	percent, _ := s.tracker.CPUPercent(key, cpu, now)

	return candidate{
		Process: proto.Process{
			PID:       pid,
			Name:      clean(st.comm, 64),
			Command:   command(cmdline),
			StartedAt: started.UTC().Truncate(time.Second),
			Usage: proto.Usage{
				CPUPercent:  percent,
				MemoryBytes: st.rssPages * s.pageSize,
			},
		},
		uid: uid,
	}, true
}

// choose se queda con los más pesados: primero los que más CPU usan (hasta
// busiestByCPU, y solo si usan algo) y después, por memoria, hasta completar
// el tope. Devuelve la lista ordenada por memoria, que es un orden que no
// salta de una foto a la otra.
func choose(all []candidate) []candidate {
	if len(all) <= proto.MaxProcesses {
		sortByMemory(all)
		return all
	}

	byCPU := slices.Clone(all)
	slices.SortStableFunc(byCPU, func(a, b candidate) int {
		return -cmpFloat(a.Usage.CPUPercent, b.Usage.CPUPercent)
	})
	chosen := make([]candidate, 0, proto.MaxProcesses)
	taken := make(map[int]bool, proto.MaxProcesses)
	for _, c := range byCPU[:busiestByCPU] {
		if c.Usage.CPUPercent < minCPUPercent {
			break
		}
		chosen = append(chosen, c)
		taken[c.PID] = true
	}

	sortByMemory(all)
	for _, c := range all {
		if len(chosen) == proto.MaxProcesses {
			break
		}
		if !taken[c.PID] {
			chosen = append(chosen, c)
		}
	}
	sortByMemory(chosen)
	return chosen
}

func sortByMemory(cs []candidate) {
	slices.SortStableFunc(cs, func(a, b candidate) int {
		switch {
		case a.Usage.MemoryBytes > b.Usage.MemoryBytes:
			return -1
		case a.Usage.MemoryBytes < b.Usage.MemoryBytes:
			return 1
		default:
			return a.PID - b.PID
		}
	})
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// stat son los campos de /proc/<pid>/stat que se usan.
type stat struct {
	comm         string
	flags        uint64
	utime, stime uint64
	startTicks   uint64
	rssPages     uint64
}

// parseStat lee /proc/<pid>/stat. El nombre va entre paréntesis y puede tener
// espacios y paréntesis adentro, así que se corta en el último ")".
func parseStat(raw string) (stat, error) {
	open := strings.IndexByte(raw, '(')
	end := strings.LastIndexByte(raw, ')')
	if open < 0 || end < open {
		return stat{}, errors.New("formato de stat desconocido")
	}
	fields := strings.Fields(raw[end+1:])
	// Después del nombre: estado(0) ppid pgrp session tty tpgid flags(6) ...
	// utime(11) stime(12) ... starttime(19) vsize(20) rss(21).
	if len(fields) < 22 {
		return stat{}, fmt.Errorf("stat con %d campos", len(fields))
	}
	var st stat
	st.comm = raw[open+1 : end]
	for _, f := range []struct {
		dst *uint64
		idx int
	}{{&st.flags, 6}, {&st.utime, 11}, {&st.stime, 12}, {&st.startTicks, 19}, {&st.rssPages, 21}} {
		v, err := strconv.ParseUint(fields[f.idx], 10, 64)
		if err != nil {
			return stat{}, fmt.Errorf("campo %d de stat: %w", f.idx, err)
		}
		*f.dst = v
	}
	return st, nil
}

// parseUID saca el UID real de /proc/<pid>/status.
func parseUID(status string) (string, bool) {
	for line := range strings.Lines(status) {
		rest, ok := strings.CutPrefix(line, "Uid:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return "", false
		}
		return fields[0], true
	}
	return "", false
}

// bootTime lee el momento de arranque de la máquina, que es la referencia de
// los tiempos de /proc/<pid>/stat.
func (s *Scanner) bootTime() (time.Time, error) {
	raw, err := fs.ReadFile(s.proc, "stat")
	if err != nil {
		return time.Time{}, fmt.Errorf("leer /proc/stat: %w", err)
	}
	for line := range strings.Lines(string(raw)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "btime "); ok {
			secs, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, fmt.Errorf("leer btime: %w", err)
			}
			return time.Unix(secs, 0), nil
		}
	}
	return time.Time{}, errors.New("/proc/stat no trae btime")
}

// users lee /etc/passwd para mostrar nombres en vez de UIDs. Si no se puede,
// quedan los UIDs, que también son válidos en el contrato.
func (s *Scanner) users() map[string]string {
	raw, err := fs.ReadFile(s.etc, "passwd")
	if err != nil {
		return nil
	}
	users := make(map[string]string)
	for line := range strings.Lines(string(raw)) {
		fields := strings.Split(strings.TrimRight(line, "\n"), ":")
		if len(fields) >= 3 && isUserName(fields[0]) {
			users[fields[2]] = fields[0]
		}
	}
	return users
}

// isUserName filtra nombres de usuario con caracteres que el contrato no
// acepta; esos quedan como UID.
func isUserName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '_' || r == '.' || r == '-' || r == '$'
		if !ok {
			return false
		}
	}
	return true
}

// command arma la línea de comando para mostrar: los argumentos separados por
// espacios, con los secretos enmascarados y recién después recortada, para
// que el recorte nunca deje media contraseña a la vista.
func command(cmdline []byte) string {
	args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	return clean(redact.String(strings.Join(args, " ")), proto.MaxCommandLen)
}

// clean deja un texto apto para el contrato: UTF-8 válido, sin caracteres de
// control y de hasta maxLen bytes sin cortar un carácter por la mitad.
func clean(s string, maxLen int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	s = strings.TrimSpace(s)
	if len(s) > maxLen {
		// Cortar por bytes puede dejar medio carácter al final; ToValidUTF8
		// lo saca.
		s = strings.TrimSpace(strings.ToValidUTF8(s[:maxLen], ""))
	}
	return s
}
