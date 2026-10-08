package templates

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JuanBaez77/status-sv/internal/projectspec"
)

// -update regenera los archivos de testdata: el diff del PR muestra
// exactamente qué cambia en lo que se escribe en los servidores.
var update = flag.Bool("update", false, "regenera los archivos de testdata")

// svdemoYAML es el proyecto de la prueba real del Sprint 6, como lo arma una
// propuesta.
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
  command: [".venv/bin/gunicorn", "app:app", "--bind", "127.0.0.1:18097", "--workers", "2"]
site:
  domain: svdemo.extendsystem.com
  port: 18097
`

func svdemo(t *testing.T) projectspec.Spec {
	t.Helper()
	spec, err := projectspec.ParseSpec("svdemo.yaml", []byte(svdemoYAML))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	return spec
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	file := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(file, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file) //nolint:gosec // ruta fija de testdata
	if err != nil {
		t.Fatalf("leer %s (¿falta correr los tests con -update?): %v", file, err)
	}
	if strings.ReplaceAll(string(want), "\r\n", "\n") != string(got) {
		t.Errorf("%s cambió.\n--- esperado ---\n%s\n--- obtenido ---\n%s", file, want, got)
	}
}

func TestGolden(t *testing.T) {
	set := Default()
	spec := svdemo(t)

	unit, err := set.Service(spec)
	if err != nil {
		t.Fatalf("Service: %v", err)
	}
	golden(t, "svdemo.service", unit)

	for name, phase := range map[string]SitePhase{"svdemo-http.conf": SiteHTTP, "svdemo-tls.conf": SiteTLS} {
		site, err := set.Site(spec, phase)
		if err != nil {
			t.Fatalf("Site: %v", err)
		}
		golden(t, name, site)
	}
}

func TestServiceQuotesArguments(t *testing.T) {
	spec := svdemo(t)
	spec.Service.Exec = "/srv/svdemo/.venv/bin/app"
	spec.Service.Command = []string{".venv/bin/app", "--name", "mi app", `"citado"`, `c:\ruta`, "100%", "$HOME", "${PATH}", "ñandú", "'simple'"}
	unit, err := Default().Service(spec)
	if err != nil {
		t.Fatalf("Service: %v", err)
	}
	want := `ExecStart="/srv/svdemo/.venv/bin/app" "--name" "mi app" "\"citado\"" "c:\\ruta" "100%%" "$$HOME" "$${PATH}" "ñandú" "'simple'"` + "\n"
	if !strings.Contains(string(unit), want) {
		t.Errorf("la unidad no tiene\n%s\ntiene:\n%s", want, unit)
	}
}

func TestServiceRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*projectspec.Spec)
	}{
		{name: "sin servicio", mutate: func(s *projectspec.Spec) { s.Service = nil }},
		{name: "docker", mutate: func(s *projectspec.Spec) { s.Project.Runtime = "docker" }},
		{name: "como root", mutate: func(s *projectspec.Spec) { s.RunAs = "root" }},
		{name: "como el agent", mutate: func(s *projectspec.Spec) { s.RunAs = projectspec.AgentUser }},
		{name: "usuario con salto de línea", mutate: func(s *projectspec.Spec) { s.RunAs = "svdemo\nExecStartPre=/bin/sh" }},
		{name: "workdir con espacio", mutate: func(s *projectspec.Spec) { s.Project.Workdir = "/srv/mi demo" }},
		{name: "workdir con especificador", mutate: func(s *projectspec.Spec) { s.Project.Workdir = "/srv/%h" }},
		{name: "env_file con espacio", mutate: func(s *projectspec.Spec) { s.EnvFile = "/etc/mi env" }},
		{name: "env_file con ..", mutate: func(s *projectspec.Spec) { s.EnvFile = "/etc/status-sv/../shadow" }},
		{name: "programa relativo", mutate: func(s *projectspec.Spec) { s.Service.Exec = "gunicorn" }},
		{name: "argumento con salto de línea", mutate: func(s *projectspec.Spec) {
			s.Service.Command = []string{".venv/bin/gunicorn", "app:app\nExecStartPost=/bin/sh"}
		}},
		{name: "argumento vacío", mutate: func(s *projectspec.Spec) { s.Service.Command = []string{".venv/bin/gunicorn", ""} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := svdemo(t)
			tt.mutate(&spec)
			if unit, err := Default().Service(spec); err == nil {
				t.Errorf("se generó la unidad:\n%s", unit)
			}
		})
	}
}

func TestSiteRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*projectspec.Spec)
	}{
		{name: "sin sitio", mutate: func(s *projectspec.Spec) { s.Site = nil }},
		{name: "dominio con directiva", mutate: func(s *projectspec.Spec) { s.Site.Domain = "a.com; location / { alias /; }" }},
		{name: "dominio con salto de línea", mutate: func(s *projectspec.Spec) { s.Site.Domain = "a.com\n" }},
		{name: "comodín", mutate: func(s *projectspec.Spec) { s.Site.Domain = "*.a.com" }},
		{name: "puerto reservado", mutate: func(s *projectspec.Spec) { s.Site.Port = 22 }},
		{name: "nombre de proyecto inválido", mutate: func(s *projectspec.Spec) { s.Project.Name = "a;b" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := svdemo(t)
			tt.mutate(&spec)
			if site, err := Default().Site(spec, SiteTLS); err == nil {
				t.Errorf("se generó el sitio:\n%s", site)
			}
		})
	}
}

// Las plantillas de un servidor reemplazan a las de fábrica, si son de root.
func TestLoad(t *testing.T) {
	trusted := func(string) error { return nil }

	empty := t.TempDir()
	set, err := Load(empty, trusted)
	if err != nil {
		t.Fatalf("Load sin reemplazos: %v", err)
	}
	if set.ServiceOrigin != Factory || set.SiteOrigin != Factory {
		t.Errorf("orígenes = %q, %q", set.ServiceOrigin, set.SiteOrigin)
	}

	dir := t.TempDir()
	custom := filepath.Join(dir, ServiceTemplate)
	if err := os.WriteFile(custom, []byte("# propia de {{.Project}}\nExecStart={{.ExecStart}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err = Load(dir, trusted)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if set.ServiceOrigin != custom || set.SiteOrigin != Factory {
		t.Errorf("orígenes = %q, %q", set.ServiceOrigin, set.SiteOrigin)
	}
	unit, err := set.Service(svdemo(t))
	if err != nil || !strings.HasPrefix(string(unit), "# propia de svdemo\n") {
		t.Errorf("Service = %q, %v", unit, err)
	}

	// Un archivo que no es de root no vuelve a la de fábrica: es un error.
	notRoot := errors.New("no es de root")
	if _, err := Load(dir, func(string) error { return notRoot }); !errors.Is(err, notRoot) {
		t.Errorf("Load con un archivo que no es de confianza = %v", err)
	}

	// Una plantilla que no compila, o que pide algo que no existe, falla.
	if err := os.WriteFile(custom, []byte("{{.Project"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, trusted); err == nil {
		t.Error("se cargó una plantilla que no compila")
	}
	if err := os.WriteFile(custom, []byte("{{.Secreto}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err = Load(dir, trusted)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := set.Service(svdemo(t)); err == nil {
		t.Error("se generó una unidad con un campo que no existe")
	}
}
