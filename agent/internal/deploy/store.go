package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/agent/internal/compose"
	"github.com/JuanBaez77/status-sv/proto"
)

// Lo que el motor guarda en disco, en /var/lib/status-sv/deploys (ADR-015):
//
//	<id>.json               el diario de cada deploy: la foto que viaja, más lo
//	                        necesario para volver atrás si el agent se reinicia.
//	projects/<nombre>.json  los últimos estados buenos de cada proyecto, que
//	                        son a donde se puede volver.
//
// Todo se escribe entero, en un temporal que después se renombra: un corte a
// mitad de camino deja el archivo anterior, nunca uno a medias.

const (
	fileMode = 0o600
	dirMode  = 0o700
	// keptJournals es cuántos diarios terminados y confirmados se conservan:
	// alcanzan para contestar igual un comando repetido.
	keptJournals = 20
	// keptStates es cuántos estados buenos se recuerdan por proyecto: el que
	// corre y dos a los que se puede volver.
	keptStates = 3
)

// GoodState es un estado del proyecto que anduvo: el commit y la imagen
// exacta de cada servicio.
type GoodState struct {
	Commit string                 `json:"commit"`
	Images []compose.ServiceImage `json:"images"`
	At     time.Time              `json:"at"`
}

// projectState son los últimos estados buenos de un proyecto, del más nuevo
// al más viejo. El primero es el que corre; el segundo, a donde vuelve un
// rollback.
type projectState struct {
	History []GoodState `json:"history"`
}

// journal es el diario de un deploy.
type journal struct {
	Event proto.DeployEvent `json:"event"`
	// AckedSeq es la última foto que el control plane confirmó.
	AckedSeq int `json:"acked_seq"`
	// Project es la declaración con la que se hizo, para poder volver atrás
	// aunque el archivo cambie mientras tanto.
	Project proto.Project `json:"project"`
	// Before es cómo estaba el proyecto antes de tocarlo. Se anota antes del
	// checkout: desde ahí en adelante, se puede volver.
	Before *GoodState `json:"before,omitempty"`
	// Touched es que ya se cambió el código o las imágenes; UpStarted, que ya
	// se recrearon contenedores.
	Touched   bool `json:"touched"`
	UpStarted bool `json:"up_started"`
}

// store lee y escribe el diario y los estados buenos.
type store struct {
	dir string
	// mu serializa las escrituras del diario. Lo escriben dos: el deploy, con
	// cada foto, y el acuse de cada foto, que llega por el canal. Sin el
	// candado, un acuse que leyó el diario antes de la foto final lo pisa
	// después con su copia vieja, y un deploy terminado vuelve a figurar en
	// curso: al próximo arranque, el agent lo volvería atrás.
	mu sync.Mutex
}

func newStore(dir string) (*store, error) {
	for _, d := range []string{dir, filepath.Join(dir, "projects")} {
		if err := os.MkdirAll(d, dirMode); err != nil {
			return nil, fmt.Errorf("crear %s: %w", d, err)
		}
	}
	return &store{dir: dir}, nil
}

func (s *store) journalPath(id uuid.UUID) string {
	return filepath.Join(s.dir, id.String()+".json")
}

func (s *store) statePath(project string) string {
	return filepath.Join(s.dir, "projects", project+".json")
}

// saveJournal guarda el diario como lo tiene el deploy. El acuse lo anota
// ackJournal por su lado, así que se conserva el más alto de los dos.
func (s *store) saveJournal(j *journal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if disk, err := s.loadJournal(j.Event.DeployID); err == nil && disk.AckedSeq > j.AckedSeq {
		j.AckedSeq = disk.AckedSeq
	}
	return writeJSON(s.journalPath(j.Event.DeployID), j)
}

// ackJournal anota que el control plane confirmó la foto seq y devuelve el
// diario como quedó. Lee y escribe con el candado tomado: lo único que cambia
// es el acuse, sobre lo último que guardó el deploy.
func (s *store) ackJournal(id uuid.UUID, seq int) (*journal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.loadJournal(id)
	if err != nil {
		return nil, err
	}
	if j.AckedSeq >= seq {
		return j, nil
	}
	j.AckedSeq = seq
	return j, writeJSON(s.journalPath(id), j)
}

func (s *store) loadJournal(id uuid.UUID) (*journal, error) {
	var j journal
	if err := readJSON(s.journalPath(id), &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// journals devuelve todos los diarios, del más viejo al más nuevo. Uno que no
// se puede leer se saltea: no puede trabar el arranque del agent.
func (s *store) journals() ([]*journal, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("leer %s: %w", s.dir, err)
	}
	var out []*journal
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		id, err := uuid.Parse(name)
		if err != nil {
			continue
		}
		j, err := s.loadJournal(id)
		if err != nil || j.Event.DeployID != id {
			continue
		}
		out = append(out, j)
	}
	slices.SortFunc(out, func(a, b *journal) int { return a.Event.StartedAt.Compare(b.Event.StartedAt) })
	return out, nil
}

// trimJournals borra los diarios terminados y confirmados más viejos, y deja
// keptJournals.
func (s *store) trimJournals() error {
	all, err := s.journals()
	if err != nil {
		return err
	}
	var done []*journal
	for _, j := range all {
		if j.Event.Status.Final() && j.AckedSeq >= j.Event.Seq {
			done = append(done, j)
		}
	}
	var errs []error
	for _, j := range done[:max(0, len(done)-keptJournals)] {
		if err := os.Remove(s.journalPath(j.Event.DeployID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *store) loadState(project string) (projectState, error) {
	var st projectState
	err := readJSON(s.statePath(project), &st)
	if errors.Is(err, fs.ErrNotExist) {
		return projectState{}, nil
	}
	return st, err
}

// pushState anota un estado bueno nuevo como el que corre, con el que había
// antes (before) como el anterior. Un commit aparece una sola vez: volver dos
// veces atrás no llena la historia con el mismo estado.
func (s *store) pushState(project string, current GoodState, before *GoodState) (projectState, error) {
	st, err := s.loadState(project)
	if err != nil {
		return projectState{}, err
	}
	next := []GoodState{current}
	candidates := st.History
	if before != nil {
		candidates = append([]GoodState{*before}, candidates...)
	}
	for _, g := range candidates {
		if len(next) >= keptStates {
			break
		}
		if !slices.ContainsFunc(next, func(n GoodState) bool { return n.Commit == g.Commit }) {
			next = append(next, g)
		}
	}
	st.History = next
	return st, writeJSON(s.statePath(project), st)
}

// commits devuelve los commits de los estados buenos, que son las imágenes de
// rollback que hay que conservar.
func (st projectState) commits() []string {
	out := make([]string, 0, len(st.History))
	for _, g := range st.History {
		out = append(out, g.Commit)
	}
	return out
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path) //nolint:gosec // rutas propias del agent, armadas con IDs y nombres validados
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("leer %s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("serializar %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".deploy-*.tmp")
	if err != nil {
		return fmt.Errorf("crear el temporal en %s: %w", dir, err)
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	if err := temp.Chmod(fileMode); err != nil {
		return fmt.Errorf("ajustar los permisos: %w", err)
	}
	if _, err := temp.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("escribir %s: %w", path, err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sincronizar %s: %w", path, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("cerrar el temporal: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("mover a %s: %w", path, err)
	}
	return nil
}
