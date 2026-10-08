package accept

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/JuanBaez77/status-sv/agent/internal/projects"
	"github.com/JuanBaez77/status-sv/internal/envfile"
)

const secret = "s3cr3t0 con $ y \"comillas\""

// envOf lee el archivo de variables de un proyecto en el servidor de mentira.
func (e *env) envOf(t *testing.T, path string) []envfile.Var {
	t.Helper()
	data, err := os.ReadFile(e.a.onDisk(path))
	if err != nil {
		t.Fatalf("leer %s: %v", path, err)
	}
	return envfile.Parse(data)
}

func value(vars []envfile.Var, name string) (string, bool) {
	for _, v := range vars {
		if v.Name == name {
			return v.Value, true
		}
	}
	return "", false
}

func TestAcceptAsksForSecrets(t *testing.T) {
	e := newEnv(t)
	e.propose(t, "svdemo", svdemoYAML)
	e.a.In = strings.NewReader("svdemo\n" + secret + "\n\n")
	if err := e.a.Accept(t.Context(), "svdemo", Options{}); err != nil {
		t.Fatalf("Accept: %v\n%s", err, e.out)
	}

	vars := e.envOf(t, "/etc/status-sv/env/svdemo.env")
	if got, ok := value(vars, "SECRET_KEY"); !ok || got != secret {
		t.Errorf("SECRET_KEY = %q, %v", got, ok)
	}
	// El valor no aparece en lo que muestra el comando.
	if strings.Contains(e.out.String(), "s3cr3t0") {
		t.Errorf("la salida muestra el valor:\n%s", e.out)
	}
	if runtime.GOOS == "linux" {
		info, err := os.Stat(e.a.onDisk("/etc/status-sv/env/svdemo.env"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("permisos del archivo de entorno: %v, %v", info.Mode(), err)
		}
		dir, err := os.Stat(e.a.onDisk("/etc/status-sv/env"))
		if err != nil || dir.Mode().Perm() != 0o700 {
			t.Errorf("permisos del directorio: %v, %v", dir.Mode(), err)
		}
	}
}

// Un valor vacío deja la variable sin cargar: el alta sigue y avisa.
func TestAcceptWithoutSecretWarns(t *testing.T) {
	e := newEnv(t)
	e.propose(t, "svdemo", svdemoYAML)
	e.a.In = strings.NewReader("svdemo\n\n\n")
	if err := e.a.Accept(t.Context(), "svdemo", Options{}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if !strings.Contains(e.out.String(), "faltan SECRET_KEY") || !strings.Contains(e.out.String(), "project env svdemo") {
		t.Errorf("no avisa lo que falta:\n%s", e.out)
	}
}

// Un valor que no se puede escribir se pide de nuevo.
func TestAcceptRetriesBadValues(t *testing.T) {
	e := newEnv(t)
	e.propose(t, "svdemo", svdemoYAML)
	e.a.In = strings.NewReader("svdemo\nit's\nbueno\n\n")
	if err := e.a.Accept(t.Context(), "svdemo", Options{}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if got, _ := value(e.envOf(t, "/etc/status-sv/env/svdemo.env"), "SECRET_KEY"); got != "bueno" {
		t.Errorf("SECRET_KEY = %q", got)
	}
}

func TestAcceptSecretsFromFile(t *testing.T) {
	e := newEnv(t)
	e.propose(t, "svdemo", svdemoYAML)
	file := filepath.Join(t.TempDir(), "valores.env")
	if err := os.WriteFile(file, []byte("SECRET_KEY=desde-archivo\nOTRA='extra'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.a.Accept(t.Context(), "svdemo", Options{Yes: true, EnvFile: file}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	vars := e.envOf(t, "/etc/status-sv/env/svdemo.env")
	if got, _ := value(vars, "SECRET_KEY"); got != "desde-archivo" {
		t.Errorf("SECRET_KEY = %q", got)
	}
	if got, _ := value(vars, "OTRA"); got != "extra" {
		t.Errorf("OTRA = %q", got)
	}
}

// Con Docker, los valores van al .env del workdir, que lee docker compose.
func TestAcceptDockerSecrets(t *testing.T) {
	e := newEnv(t)
	yaml := dockerYAML + "required_env: [SECRET_KEY]\n"
	e.propose(t, "tienda", yaml)
	e.a.In = strings.NewReader("tienda\nvalor-docker\n")
	if err := e.a.Accept(t.Context(), "tienda", Options{NoDeployKey: true}); err != nil {
		t.Fatalf("Accept: %v\n%s", err, e.out)
	}
	if got, _ := value(e.envOf(t, "/srv/tienda/.env"), "SECRET_KEY"); got != "valor-docker" {
		t.Errorf("SECRET_KEY = %q", got)
	}
	if runtime.GOOS == "linux" {
		info, err := os.Stat(e.a.onDisk("/srv/tienda/.env"))
		if err != nil || info.Mode().Perm() != 0o640 {
			t.Errorf("permisos del .env: %v, %v", info.Mode(), err)
		}
	}
}

// Un .env que es un enlace simbólico no se lee ni se toca.
func TestAcceptRefusesSymlinkedEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("los enlaces simbólicos piden permisos especiales en Windows")
	}
	e := newEnv(t)
	e.propose(t, "tienda", dockerYAML+"required_env: [SECRET_KEY]\n")
	workdir := e.a.onDisk("/srv/tienda")
	if err := os.MkdirAll(workdir, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "otro")
	if err := os.WriteFile(target, []byte("NO_TOCAR=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(workdir, ".env")); err != nil {
		t.Fatal(err)
	}
	e.a.In = strings.NewReader("tienda\nx\n")
	if err := e.a.Accept(t.Context(), "tienda", Options{NoDeployKey: true}); err == nil || !strings.Contains(err.Error(), "no es un archivo regular") {
		t.Fatalf("Accept = %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "NO_TOCAR=1\n" {
		t.Errorf("se tocó el archivo de destino: %q", data)
	}
}

// project env cambia los valores de un proyecto ya declarado; Enter deja el
// que había.
func TestEnvCommand(t *testing.T) {
	e := newEnv(t)
	e.propose(t, "svdemo", strings.Replace(svdemoYAML, "required_env: [SECRET_KEY]", "required_env: [SECRET_KEY, DATABASE_URL]", 1))
	e.a.In = strings.NewReader("svdemo\nprimero\npostgres://db\n\n")
	if err := e.a.Accept(t.Context(), "svdemo", Options{}); err != nil {
		t.Fatalf("Accept: %v\n%s", err, e.out)
	}

	e.a.LoadSpec = projects.ReadSpec
	e.a.In, e.a.in = strings.NewReader("segundo\n\n"), nil
	if err := e.a.Env(t.Context(), "svdemo", ""); err != nil {
		t.Fatalf("Env: %v\n%s", err, e.out)
	}
	vars := e.envOf(t, "/etc/status-sv/env/svdemo.env")
	if got, _ := value(vars, "SECRET_KEY"); got != "segundo" {
		t.Errorf("SECRET_KEY = %q", got)
	}
	if got, _ := value(vars, "DATABASE_URL"); got != "postgres://db" {
		t.Errorf("DATABASE_URL = %q (Enter tenía que dejarla)", got)
	}

	e.sys.root = false
	if err := e.a.Env(t.Context(), "svdemo", ""); !errors.Is(err, ErrNotRoot) {
		t.Errorf("sin root = %v", err)
	}
}
