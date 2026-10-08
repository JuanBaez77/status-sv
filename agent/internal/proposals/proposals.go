// Package proposals guarda las propuestas de proyectos nuevos que manda el
// control plane (ADR-022), hasta que una persona las acepta en el servidor.
//
// Una propuesta no es un comando: el agent no ejecuta nada con ella. La valida
// con el parser estricto del project.yaml y con lo que una propuesta no elige,
// y la deja en su directorio de estado, que solo él y root leen. La declara
// `statussv-agent project accept`, que corre como root y la vuelve a validar:
// para ese comando, lo que hay acá es texto no confiable.
package proposals

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/internal/projectspec"
	"github.com/JuanBaez77/status-sv/proto"
)

// DirName es el directorio de las propuestas, adentro del de estado del agent.
const DirName = "proposals"

// Meta es lo que se guarda de una propuesta además de su declaración.
type Meta struct {
	ProposalID uuid.UUID `json:"proposal_id"`
	Name       string    `json:"name"`
	Hash       string    `json:"hash"`
	ExpiresAt  time.Time `json:"expires_at"`
	ReceivedAt time.Time `json:"received_at"`
}

// Proposal es una propuesta guardada: su declaración, tal cual llegó, y lo
// demás.
type Proposal struct {
	Meta
	YAML []byte
}

// Store es el directorio de propuestas. Lo usan a la vez el canal (que guarda
// y retira) y la lectura de proyectos (que las lista).
type Store struct {
	dir string
	// declared dice si ya hay un proyecto declarado con ese nombre.
	declared func(name string) bool
	now      func() time.Time

	mu sync.Mutex
}

// NewStore arma el almacén sobre dir. declared es cómo se sabe si un nombre ya
// está declarado en el servidor: una propuesta con ese nombre se rechaza.
func NewStore(dir string, declared func(name string) bool) *Store {
	return &Store{dir: dir, declared: declared, now: time.Now}
}

// Handle atiende un project_proposal y devuelve la respuesta.
func (s *Store) Handle(p proto.ProjectProposal) proto.ProjectProposalAck {
	ack := proto.ProjectProposalAck{ProposalID: p.ProposalID, Name: p.Name}
	var err error
	switch p.Action {
	case proto.ProposalPut:
		err = s.put(p)
		ack.Result = proto.ProposalStored
	case proto.ProposalWithdraw:
		err = s.withdraw(p.ProposalID, p.Name)
		ack.Result = proto.ProposalWithdrawn
	default:
		err = errors.New("acción desconocida")
	}
	if err != nil {
		ack.Result, ack.Error = proto.ProposalRejected, clean(err.Error())
	}
	return ack
}

// ErrFull es que ya hay el máximo de propuestas guardadas.
var ErrFull = fmt.Errorf("ya hay %d propuestas esperando en este servidor; hay que aceptar o retirar alguna", proto.MaxPendingProposals)

func (s *Store) put(p proto.ProjectProposal) error {
	if err := p.Validate(); err != nil {
		return err
	}
	now := s.now()
	if !p.ExpiresAt.After(now) {
		return errors.New("la propuesta ya venció")
	}
	data := []byte(p.YAML)
	spec, err := projectspec.ParseSpec(p.Name+".yaml", data)
	if err != nil {
		return err
	}
	if err := projectspec.CheckProposal(spec); err != nil {
		return err
	}
	if s.declared(p.Name) {
		return errors.New("ya hay un proyecto declarado con ese nombre en este servidor")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	pending, err := s.list(now)
	if err != nil {
		return err
	}
	if len(pending) >= proto.MaxPendingProposals && !slices.ContainsFunc(pending, func(m Meta) bool { return m.Name == p.Name }) {
		return ErrFull
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("no se pudo crear el directorio de propuestas: %w", err)
	}
	meta := Meta{ProposalID: p.ProposalID, Name: p.Name, Hash: proto.SpecHash(data), ExpiresAt: p.ExpiresAt.UTC(), ReceivedAt: now.UTC()}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	// Primero la declaración y después la ficha: la ficha es la que dice que
	// la propuesta está completa.
	if err := writeAtomic(s.yamlPath(p.Name), data); err != nil {
		return err
	}
	return writeAtomic(s.metaPath(p.Name), raw)
}

func (s *Store) withdraw(id uuid.UUID, name string) error {
	if !proto.ValidProjectName(name) {
		return errors.New("nombre de proyecto inválido")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, err := readMeta(s.metaPath(name))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err == nil && meta.ProposalID != id:
		// Hay una propuesta más nueva con el mismo nombre: esa no se retiró.
		return nil
	}
	return s.remove(name)
}

// List devuelve las propuestas guardadas, por nombre, y borra las vencidas o
// rotas.
func (s *Store) List() ([]proto.PendingProposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	metas, err := s.list(s.now())
	if err != nil {
		return nil, err
	}
	pending := make([]proto.PendingProposal, 0, len(metas))
	for _, m := range metas {
		// Una propuesta cuyo nombre ya está declarado se aceptó (o alguien
		// lo declaró a mano): ya no espera a nadie. Queda en el disco hasta
		// que project accept termine y la borre, o hasta que venza: si el
		// alta quedó por la mitad, correrlo de nuevo la necesita.
		if s.declared(m.Name) {
			continue
		}
		pending = append(pending, proto.PendingProposal{ProposalID: m.ProposalID, Name: m.Name, Hash: m.Hash})
	}
	return pending, nil
}

// Remove borra una propuesta de dir. Lo usa project accept, como root, cuando
// el alta terminó.
func Remove(dir, name string) error {
	if !proto.ValidProjectName(name) {
		return errors.New("nombre de proyecto inválido")
	}
	return (&Store{dir: dir}).remove(name)
}

// list lee las fichas del directorio. Se llama con s.mu tomado.
func (s *Store) list(now time.Time) ([]Meta, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("no se pudo leer el directorio de propuestas: %w", err)
	}
	var metas []Meta
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !proto.ValidProjectName(name) {
			continue
		}
		p, err := Load(s.dir, name)
		if err != nil || !p.ExpiresAt.After(now) {
			// Vencida, a medio escribir o tocada por alguien: no se ofrece.
			_ = s.remove(name)
			continue
		}
		metas = append(metas, p.Meta)
	}
	slices.SortFunc(metas, func(a, b Meta) int { return strings.Compare(a.Name, b.Name) })
	return metas, nil
}

// Load lee una propuesta guardada en dir y verifica que la declaración sea la
// que dice su ficha. Lo usa también el comando de aceptación, como root: no
// dice nada sobre si la declaración es válida, eso lo revisa quien la usa.
func Load(dir, name string) (Proposal, error) {
	if !proto.ValidProjectName(name) {
		return Proposal{}, errors.New("nombre de proyecto inválido")
	}
	meta, err := readMeta(filepath.Join(dir, name+".json"))
	if err != nil {
		return Proposal{}, err
	}
	data, err := readLimited(filepath.Join(dir, name+".yaml"), proto.MaxProposalBytes)
	if err != nil {
		return Proposal{}, err
	}
	if meta.Name != name || proto.SpecHash(data) != meta.Hash {
		return Proposal{}, errors.New("la propuesta guardada no coincide con su ficha")
	}
	return Proposal{Meta: meta, YAML: data}, nil
}

func readMeta(path string) (Meta, error) {
	raw, err := readLimited(path, 4<<10)
	if err != nil {
		return Meta{}, err
	}
	var meta Meta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return Meta{}, fmt.Errorf("ficha de propuesta ilegible: %w", err)
	}
	return meta, nil
}

// readLimited lee un archivo regular de hasta limit bytes. No sigue enlaces
// simbólicos: el comando de aceptación lee este directorio como root.
func readLimited(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s no es un archivo regular", filepath.Base(path))
	}
	f, err := os.Open(path) //nolint:gosec // ruta armada con un nombre de proyecto validado
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s pesa más de %d bytes", filepath.Base(path), limit)
	}
	return data, nil
}

// remove borra la propuesta. Se llama con s.mu tomado.
func (s *Store) remove(name string) error {
	for _, path := range []string{s.metaPath(name), s.yamlPath(name)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no se pudo borrar la propuesta: %w", err)
		}
	}
	return nil
}

func (s *Store) yamlPath(name string) string { return filepath.Join(s.dir, name+".yaml") }
func (s *Store) metaPath(name string) string { return filepath.Join(s.dir, name+".json") }

// writeAtomic escribe un archivo entero o nada: primero uno temporal y después
// lo renombra.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("no se pudo guardar la propuesta: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("no se pudo guardar la propuesta: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("no se pudo guardar la propuesta: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("no se pudo guardar la propuesta: %w", err)
	}
	return nil
}

// clean deja un error dentro del contrato: imprimible y corto.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	if len(s) > proto.MaxProjectErrorLen {
		s = strings.ToValidUTF8(s[:proto.MaxProjectErrorLen], "")
	}
	return s
}
