package projects

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/proto"
)

var t0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

const cristales = `name: cristales-baez
environment: production
repo: github.com/extendsystem/cristales-baez
branch: main
runtime: docker
compose_file: docker-compose.yml
workdir: /srv/cristales-baez
`

const facturas = `name: facturas
environment: staging
repo: github.com/extendsystem/facturas
branch: main
runtime: systemd
workdir: /srv/facturas
units: [gunicorn@facturas.service]
`

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadDirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "cristales-baez.yaml", cristales)
	write(t, dir, "facturas.yml", facturas)
	write(t, dir, "roto.yaml", "name: roto\nclave: DATABASE_PASSWORD=s3cr3t0\n")
	write(t, dir, "README.md", "no es un proyecto")
	write(t, dir, ".oculto.yaml", cristales)
	write(t, dir, "cristales-baez.yaml.bak", cristales)
	if err := os.Mkdir(filepath.Join(dir, "sub.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}

	state := Read(dir, t0)
	if err := state.Validate(); err != nil {
		t.Fatalf("la foto no cumple el contrato: %v", err)
	}
	if state.Source.Status != proto.SourceOK || !state.TakenAt.Equal(t0) {
		t.Errorf("fuente = %+v, marca %v", state.Source, state.TakenAt)
	}
	if len(state.Projects) != 2 || state.Projects[0].Name != "cristales-baez" || state.Projects[1].Name != "facturas" {
		t.Errorf("proyectos = %+v", state.Projects)
	}
	if len(state.Invalid) != 2 {
		t.Fatalf("inválidos = %+v; se esperaban roto.yaml y el directorio sub.yaml", state.Invalid)
	}
	for _, invalid := range state.Invalid {
		if strings.Contains(invalid.Error, "s3cr3t0") {
			t.Errorf("el error de %s muestra el contenido: %s", invalid.File, invalid.Error)
		}
	}
}

func TestReadMissingDirectoryIsUnavailable(t *testing.T) {
	state := Read(filepath.Join(t.TempDir(), "no-existe"), t0)
	if state.Source.Status != proto.SourceUnavailable || len(state.Projects) != 0 {
		t.Errorf("foto = %+v", state)
	}
	if err := state.Validate(); err != nil {
		t.Errorf("la foto no cumple el contrato: %v", err)
	}
}

func TestReadRejectsSymlinksAndDuplicates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("los enlaces simbólicos necesitan permisos especiales en Windows")
	}
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "afuera.yaml")
	if err := os.WriteFile(outside, []byte(cristales), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "cristales-baez.yaml")); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "facturas.yaml", facturas)
	write(t, dir, "facturas.yml", facturas)

	state := Read(dir, t0)
	if len(state.Projects) != 0 {
		t.Errorf("se aceptaron proyectos que no correspondían: %+v", state.Projects)
	}
	if len(state.Invalid) != 3 {
		t.Errorf("inválidos = %+v; se esperaba el enlace y los dos archivos con el mismo nombre", state.Invalid)
	}
}

func TestReadTooManyFiles(t *testing.T) {
	dir := t.TempDir()
	for i := range proto.MaxProjects + 5 {
		name := "p" + strings.Repeat("x", i%3) + string(rune('a'+i/26)) + string(rune('a'+i%26))
		write(t, dir, name+".yaml", strings.Replace(facturas, "name: facturas", "name: "+name, 1))
	}
	state := Read(dir, t0)
	if err := state.Validate(); err != nil {
		t.Fatalf("con demasiados archivos la foto no cumple el contrato: %v", err)
	}
	if len(state.Projects) != proto.MaxProjects || len(state.Invalid) != 5 {
		t.Errorf("proyectos %d, inválidos %d", len(state.Projects), len(state.Invalid))
	}
}

func TestRunnerSendsOnlyChanges(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "facturas.yaml", facturas)
	box := NewOutbox()
	r := NewRunner(dir, box, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := t0
	r.now = func() time.Time { return now }

	r.take(t.Context())
	if _, ok := box.Next(now); !ok {
		t.Fatal("la primera lectura no dejó una foto")
	}
	first, _ := box.Next(now)
	box.Sent([16]byte{1}, first.TakenAt, now)
	box.Acked(first.TakenAt)

	// Sin cambios, no sale nada.
	now = now.Add(DefaultRefresh)
	r.take(t.Context())
	if _, ok := box.Next(now); ok {
		t.Error("salió una foto sin cambios")
	}

	// Un archivo nuevo sí.
	write(t, dir, "cristales-baez.yaml", cristales)
	now = now.Add(DefaultRefresh)
	r.take(t.Context())
	s, ok := box.Next(now)
	if !ok || len(s.Projects) != 2 {
		t.Fatalf("después de agregar un archivo = %+v, %v", s, ok)
	}
	box.Sent([16]byte{2}, s.TakenAt, now)
	box.Acked(s.TakenAt)
	if got := r.Current(); len(got) != 2 {
		t.Errorf("Current = %d proyectos", len(got))
	}

	// Pasados 5 minutos sale igual, aunque no cambie nada.
	now = now.Add(DefaultResend)
	r.take(t.Context())
	if _, ok := box.Next(now); !ok {
		t.Error("no salió la foto de los 5 minutos")
	}
}

func TestClean(t *testing.T) {
	if got := clean("a/b\x1b[31m.yaml", 128); strings.ContainsAny(got, "/\x1b") {
		t.Errorf("clean dejó caracteres peligrosos: %q", got)
	}
	if got := clean(strings.Repeat("ñ", 100), 9); len(got) > 9 || !strings.HasPrefix(got, "ññññ") {
		t.Errorf("clean cortó mal: %q", got)
	}
}

func TestReadSpec(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "cristales-baez.yml", cristales+"deploy:\n  pre:\n    - service: backend\n      command: [python, manage.py, migrate]\n")
	write(t, dir, "facturas.yaml", facturas)
	write(t, dir, "roto.yaml", "name: roto\n")

	spec, err := ReadSpec(dir, "cristales-baez")
	if err != nil {
		t.Fatalf("ReadSpec: %v", err)
	}
	if spec.Project.ComposeProject != "cristales-baez" || len(spec.Pre) != 1 || spec.Pre[0].Service != "backend" {
		t.Errorf("spec = %+v", spec)
	}
	if _, err := ReadSpec(dir, "facturas"); err != nil {
		t.Errorf("facturas: %v", err)
	}
	for _, name := range []string{"no-existe", "../etc/passwd", "", "Cristales"} {
		if _, err := ReadSpec(dir, name); !errors.Is(err, ErrNotDeclared) {
			t.Errorf("ReadSpec(%q) = %v, se esperaba ErrNotDeclared", name, err)
		}
	}
	if _, err := ReadSpec(dir, "roto"); err == nil || errors.Is(err, ErrNotDeclared) {
		t.Errorf("un archivo inválido tiene que dar su error: %v", err)
	}

	write(t, dir, "facturas.yml", facturas)
	if _, err := ReadSpec(dir, "facturas"); err == nil {
		t.Error("con .yaml y .yml a la vez no tendría que valer ninguno")
	}
}

func TestReadSpecRejectsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("los enlaces simbólicos necesitan permisos especiales en Windows")
	}
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "afuera.yaml")
	if err := os.WriteFile(outside, []byte(cristales), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "cristales-baez.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSpec(dir, "cristales-baez"); err == nil {
		t.Fatal("ReadSpec siguió un enlace simbólico")
	}
}
