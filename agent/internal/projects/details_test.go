package projects

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/proto"
)

// fakeEnvChecker contesta lo que se le diga y cuenta las preguntas.
type fakeEnvChecker struct {
	missing []string
	checked bool
	calls   int
}

func (f *fakeEnvChecker) CheckEnv(context.Context, string) ([]string, bool, error) {
	f.calls++
	return f.missing, f.checked, nil
}

const facturasEnv = facturas + "run_as: facturas\nenv_file: /etc/status-sv/env/facturas.env\nrequired_env: [SECRET_KEY, DATABASE_URL]\n"

func TestDetailsSystemd(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "facturas.yaml", facturasEnv)
	helper := &fakeEnvChecker{missing: []string{"DATABASE_URL"}, checked: true}
	d := NewDetails(dir, "", helper)
	now := t0
	d.now = func() time.Time { return now }

	p := proto.Project{Name: "facturas", Runtime: proto.RuntimeSystemd, SpecHash: "a"}
	d.Fill(t.Context(), &p)
	if p.Env == nil || p.Env.Required != 2 || strings.Join(p.Env.Missing, ",") != "DATABASE_URL" || p.Env.Unknown {
		t.Fatalf("Env = %+v", p.Env)
	}

	// Unos minutos después no vuelve a preguntar: cada pregunta es un
	// proceso root.
	now = now.Add(time.Minute)
	p = proto.Project{Name: "facturas", Runtime: proto.RuntimeSystemd, SpecHash: "a"}
	d.Fill(t.Context(), &p)
	if helper.calls != 1 {
		t.Errorf("preguntas = %d", helper.calls)
	}
	// Con la declaración cambiada, o pasado el rato, sí.
	p = proto.Project{Name: "facturas", Runtime: proto.RuntimeSystemd, SpecHash: "b"}
	d.Fill(t.Context(), &p)
	now = now.Add(envRecheck)
	d.Fill(t.Context(), &p)
	if helper.calls != 3 {
		t.Errorf("preguntas = %d", helper.calls)
	}

	// Si el ayudante no pudo mirar, queda como no mirado, sin nombres.
	unknown := NewDetails(dir, "", &fakeEnvChecker{})
	p = proto.Project{Name: "facturas", Runtime: proto.RuntimeSystemd}
	unknown.Fill(t.Context(), &p)
	if p.Env == nil || !p.Env.Unknown || len(p.Env.Missing) != 0 {
		t.Errorf("sin ayudante = %+v", p.Env)
	}
	if err := p.Env.Validate(); err != nil {
		t.Errorf("no entra en el contrato: %v", err)
	}
}

func TestDetailsDocker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("el workdir de un proyecto es una ruta de Linux")
	}
	dir := t.TempDir()
	workdir := t.TempDir()
	yaml := strings.Replace(cristales, "/srv/cristales-baez", workdir, 1) + "required_env: [SECRET_KEY, DATABASE_URL]\n"
	write(t, dir, "cristales-baez.yaml", yaml)
	d := NewDetails(dir, "", nil)

	p := proto.Project{Name: "cristales-baez", Runtime: proto.RuntimeDocker}
	d.Fill(t.Context(), &p)
	if p.Env == nil || len(p.Env.Missing) != 2 {
		t.Fatalf("sin .env = %+v", p.Env)
	}

	if err := os.WriteFile(filepath.Join(workdir, ".env"), []byte("SECRET_KEY='s3cr3t0'\nDATABASE_URL=''\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p = proto.Project{Name: "cristales-baez", Runtime: proto.RuntimeDocker}
	d.Fill(t.Context(), &p)
	if p.Env == nil || strings.Join(p.Env.Missing, ",") != "DATABASE_URL" {
		t.Errorf("con .env = %+v", p.Env)
	}
	if raw := strings.Join(p.Env.Missing, ","); strings.Contains(raw, "s3cr3t0") {
		t.Error("viajó un valor")
	}

	// Un .env que es un enlace simbólico no se sigue.
	if err := os.Remove(filepath.Join(workdir, ".env")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(workdir, ".env")); err != nil {
		t.Fatal(err)
	}
	p = proto.Project{Name: "cristales-baez", Runtime: proto.RuntimeDocker}
	d.Fill(t.Context(), &p)
	if p.Env == nil || !p.Env.Unknown {
		t.Errorf("con un enlace = %+v", p.Env)
	}
}

func TestDetailsPublicKey(t *testing.T) {
	dir := t.TempDir()
	keys := t.TempDir()
	write(t, dir, "facturas.yaml", facturas)
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	write(t, keys, "facturas.pub", key+" status-sv facturas@vps\n")
	d := NewDetails(dir, keys, nil)

	p := proto.Project{Name: "facturas", Runtime: proto.RuntimeSystemd}
	d.Fill(t.Context(), &p)
	if p.DeployKey != key {
		t.Errorf("DeployKey = %q", p.DeployKey)
	}
	// Sin variables requeridas, no hay estado de variables.
	if p.Env != nil {
		t.Errorf("Env = %+v", p.Env)
	}

	write(t, keys, "facturas.pub", "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n")
	p = proto.Project{Name: "facturas", Runtime: proto.RuntimeSystemd}
	d.Fill(t.Context(), &p)
	if p.DeployKey != "" {
		t.Errorf("se mandó algo que no es una llave pública: %q", p.DeployKey)
	}
}
