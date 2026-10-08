package proposals

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

const svdemoYAML = `name: svdemo
environment: staging
repo: github.com/JuanBaez77/status-sv-demo
branch: main
runtime: systemd
workdir: /srv/svdemo
run_as: svdemo
env_file: /etc/status-sv/env/svdemo.env
required_env: [SECRET_KEY]
service:
  command: [.venv/bin/gunicorn, app:app, --bind, 127.0.0.1:18097]
site:
  domain: svdemo.extendsystem.com
  port: 18097
`

var start = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// newStore arma un almacén en un directorio de prueba, con un reloj que se
// puede mover y los nombres que se den por declarados.
func newStore(t *testing.T, declared ...string) (*Store, *time.Time) {
	t.Helper()
	now := start
	s := NewStore(filepath.Join(t.TempDir(), DirName), func(name string) bool {
		for _, d := range declared {
			if d == name {
				return true
			}
		}
		return false
	})
	s.now = func() time.Time { return now }
	return s, &now
}

func put(name, yaml string) proto.ProjectProposal {
	return proto.ProjectProposal{
		ProposalID: uuid.New(),
		Name:       name,
		Action:     proto.ProposalPut,
		YAML:       yaml,
		ExpiresAt:  start.Add(7 * 24 * time.Hour),
	}
}

func list(t *testing.T, s *Store) []proto.PendingProposal {
	t.Helper()
	pending, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return pending
}

func TestPutAndList(t *testing.T) {
	s, _ := newStore(t)
	p := put("svdemo", svdemoYAML)
	ack := s.Handle(p)
	if ack.Result != proto.ProposalStored || ack.ProposalID != p.ProposalID || ack.Name != "svdemo" {
		t.Fatalf("ack = %+v", ack)
	}
	if err := ack.Validate(); err != nil {
		t.Errorf("el acuse no entra en el contrato: %v", err)
	}

	pending := list(t, s)
	if len(pending) != 1 || pending[0].ProposalID != p.ProposalID || pending[0].Hash != proto.SpecHash([]byte(svdemoYAML)) {
		t.Fatalf("List = %+v", pending)
	}

	// Lo que se guarda es exactamente lo que llegó.
	got, err := Load(s.dir, "svdemo")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got.YAML) != svdemoYAML || got.ExpiresAt != p.ExpiresAt || got.ReceivedAt != start {
		t.Errorf("Load = %+v", got.Meta)
	}
	if runtime.GOOS == "linux" {
		for _, file := range []string{"svdemo.yaml", "svdemo.json"} {
			info, err := os.Stat(filepath.Join(s.dir, file))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Errorf("%s: %v, permisos %v", file, err, info.Mode().Perm())
			}
		}
	}

	// La misma propuesta otra vez da lo mismo; una nueva con el mismo nombre
	// reemplaza a la anterior.
	if ack := s.Handle(p); ack.Result != proto.ProposalStored {
		t.Errorf("repetida: %+v", ack)
	}
	newer := put("svdemo", strings.Replace(svdemoYAML, "port: 18097", "port: 18099", 1))
	if ack := s.Handle(newer); ack.Result != proto.ProposalStored {
		t.Fatalf("reemplazo: %+v", ack)
	}
	if pending := list(t, s); len(pending) != 1 || pending[0].ProposalID != newer.ProposalID {
		t.Errorf("después del reemplazo: %+v", pending)
	}
}

func TestPutRejects(t *testing.T) {
	tests := []struct {
		name     string
		proposal proto.ProjectProposal
		declared []string
		wantErr  string
	}{
		{name: "no es YAML", proposal: put("svdemo", "name: [svdemo"), wantErr: "YAML inválido"},
		{name: "otro nombre adentro", proposal: put("otro", svdemoYAML), wantErr: "name"},
		{name: "workdir elegido", proposal: put("svdemo", strings.ReplaceAll(svdemoYAML, "/srv/svdemo", "/opt/svdemo")), wantErr: "workdir"},
		{name: "usuario elegido", proposal: put("svdemo", strings.Replace(svdemoYAML, "run_as: svdemo", "run_as: postgres", 1)), wantErr: "run_as"},
		{name: "archivo de entorno elegido", proposal: put("svdemo", strings.Replace(svdemoYAML, "/etc/status-sv/env/svdemo.env", "/etc/shadow", 1)), wantErr: "env_file"},
		{name: "ya declarado", proposal: put("svdemo", svdemoYAML), declared: []string{"svdemo"}, wantErr: "ya hay un proyecto declarado"},
		{name: "vencida", proposal: func() proto.ProjectProposal { p := put("svdemo", svdemoYAML); p.ExpiresAt = start; return p }(), wantErr: "venció"},
		{name: "sin ID", proposal: func() proto.ProjectProposal { p := put("svdemo", svdemoYAML); p.ProposalID = uuid.Nil; return p }(), wantErr: "ID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newStore(t, tt.declared...)
			ack := s.Handle(tt.proposal)
			if ack.Result != proto.ProposalRejected || !strings.Contains(ack.Error, tt.wantErr) {
				t.Errorf("ack = %+v, se esperaba un rechazo que dijera %q", ack, tt.wantErr)
			}
			if tt.proposal.ProposalID != uuid.Nil {
				if err := ack.Validate(); err != nil {
					t.Errorf("el acuse no entra en el contrato: %v", err)
				}
			}
			if pending := list(t, s); len(pending) != 0 {
				t.Errorf("quedó guardada: %+v", pending)
			}
		})
	}
}

// El rechazo no muestra los valores de la declaración: viaja al control plane.
func TestRejectionNeverIncludesValues(t *testing.T) {
	s, _ := newStore(t)
	const secret = "s3cr3t0"
	ack := s.Handle(put("svdemo", strings.Replace(svdemoYAML, "SECRET_KEY", secret+"=x", 1)))
	if ack.Result != proto.ProposalRejected || strings.Contains(ack.Error, secret) {
		t.Errorf("ack = %+v", ack)
	}
}

func TestFull(t *testing.T) {
	s, _ := newStore(t)
	names := make([]string, 0, proto.MaxPendingProposals)
	for i := range proto.MaxPendingProposals {
		name := "demo" + string(rune('a'+i))
		names = append(names, name)
		if ack := s.Handle(put(name, strings.ReplaceAll(svdemoYAML, "svdemo", name))); ack.Result != proto.ProposalStored {
			t.Fatalf("%s: %+v", name, ack)
		}
	}
	extra := put("otra", strings.ReplaceAll(svdemoYAML, "svdemo", "otra"))
	if ack := s.Handle(extra); ack.Result != proto.ProposalRejected || !strings.Contains(ack.Error, "ya hay") {
		t.Errorf("una de más: %+v", ack)
	}
	// Reemplazar una de las que ya están sí se puede.
	if ack := s.Handle(put(names[0], strings.ReplaceAll(svdemoYAML, "svdemo", names[0]))); ack.Result != proto.ProposalStored {
		t.Errorf("reemplazo con el máximo: %+v", ack)
	}
}

func TestWithdraw(t *testing.T) {
	s, _ := newStore(t)
	p := put("svdemo", svdemoYAML)
	s.Handle(p)

	// Retirar otra propuesta con el mismo nombre no toca la guardada.
	other := proto.ProjectProposal{ProposalID: uuid.New(), Name: "svdemo", Action: proto.ProposalWithdraw}
	if ack := s.Handle(other); ack.Result != proto.ProposalWithdrawn {
		t.Errorf("otra: %+v", ack)
	}
	if len(list(t, s)) != 1 {
		t.Fatal("se retiró una propuesta más nueva")
	}

	withdraw := proto.ProjectProposal{ProposalID: p.ProposalID, Name: "svdemo", Action: proto.ProposalWithdraw}
	if ack := s.Handle(withdraw); ack.Result != proto.ProposalWithdrawn {
		t.Errorf("retirar: %+v", ack)
	}
	if pending := list(t, s); len(pending) != 0 {
		t.Errorf("después de retirar: %+v", pending)
	}
	// Retirar lo que no está es lo mismo: el control plane puede repetirlo.
	if ack := s.Handle(withdraw); ack.Result != proto.ProposalWithdrawn {
		t.Errorf("retirar de nuevo: %+v", ack)
	}
}

// Las vencidas y las que alguien tocó no se ofrecen, y se borran.
func TestListDropsExpiredAndTampered(t *testing.T) {
	s, now := newStore(t)
	s.Handle(put("svdemo", svdemoYAML))
	other := strings.ReplaceAll(svdemoYAML, "svdemo", "otra")
	s.Handle(put("otra", other))

	// Alguien cambia la declaración guardada: ya no coincide con su ficha.
	if err := os.WriteFile(filepath.Join(s.dir, "otra.yaml"), []byte(other+"# cambio\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pending := list(t, s); len(pending) != 1 || pending[0].Name != "svdemo" {
		t.Fatalf("List = %+v", pending)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "otra.yaml")); !os.IsNotExist(err) {
		t.Errorf("la propuesta tocada sigue en el disco: %v", err)
	}

	*now = start.Add(8 * 24 * time.Hour)
	if pending := list(t, s); len(pending) != 0 {
		t.Errorf("vencida: %+v", pending)
	}
	if entries, _ := os.ReadDir(s.dir); len(entries) != 0 {
		t.Errorf("quedaron archivos: %v", entries)
	}
}

func TestListWithoutDirectory(t *testing.T) {
	s, _ := newStore(t)
	if pending := list(t, s); len(pending) != 0 {
		t.Errorf("List = %+v", pending)
	}
}

// Load no sigue enlaces simbólicos: el comando de aceptación lo usa como root.
func TestLoadRejectsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("los enlaces simbólicos piden permisos especiales en Windows")
	}
	s, _ := newStore(t)
	s.Handle(put("svdemo", svdemoYAML))
	target := filepath.Join(t.TempDir(), "otro.yaml")
	if err := os.WriteFile(target, []byte(svdemoYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := filepath.Join(s.dir, "svdemo.yaml")
	if err := os.Remove(yaml); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, yaml); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(s.dir, "svdemo"); err == nil {
		t.Error("Load siguió un enlace simbólico")
	}
	if _, err := Load(s.dir, "../svdemo"); err == nil {
		t.Error("Load aceptó un nombre con ..")
	}
}

// Una propuesta cuyo nombre ya se declaró no se informa, pero queda en el
// disco: si el alta quedó por la mitad, project accept la necesita de nuevo.
func TestListOmitsDeclared(t *testing.T) {
	declared := false
	s := NewStore(filepath.Join(t.TempDir(), DirName), func(string) bool { return declared })
	s.now = func() time.Time { return start }
	s.Handle(put("svdemo", svdemoYAML))
	declared = true
	if pending := list(t, s); len(pending) != 0 {
		t.Errorf("List = %+v", pending)
	}
	if _, err := Load(s.dir, "svdemo"); err != nil {
		t.Errorf("la propuesta se borró: %v", err)
	}
	if err := Remove(s.dir, "svdemo"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := Load(s.dir, "svdemo"); !os.IsNotExist(err) {
		t.Errorf("después de Remove: %v", err)
	}
}
