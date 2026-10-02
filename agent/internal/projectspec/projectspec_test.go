package projectspec

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/proto"
)

const dockerYAML = `name: cristales-baez
environment: production
repo: github.com/extendsystem/cristales-baez
branch: main
runtime: docker
compose_file: docker-compose.yml
workdir: /srv/cristales-baez
healthcheck:
  url: https://cristales.example.com/health
  expect_status: 200
  timeout_seconds: 5
deploy:
  pre: []
  post: []
rollback: previous_image
`

const systemdYAML = `name: facturas
environment: staging
repo: git@github.com:extendsystem/facturas.git
branch: main
runtime: systemd
workdir: /srv/facturas
units:
  - gunicorn@facturas.service
  - facturas-worker.service
`

func TestParseDocker(t *testing.T) {
	project, err := Parse("cristales-baez.yaml", []byte(dockerYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := proto.Project{
		Name:           "cristales-baez",
		Environment:    proto.EnvProduction,
		Repo:           "github.com/extendsystem/cristales-baez",
		Branch:         "main",
		Runtime:        proto.RuntimeDocker,
		Workdir:        "/srv/cristales-baez",
		ComposeFile:    "docker-compose.yml",
		ComposeProject: "cristales-baez",
		Healthcheck:    &proto.HealthcheckSpec{URL: "https://cristales.example.com/health", ExpectStatus: 200, TimeoutSeconds: 5},
	}
	if project.Name != want.Name || project.ComposeProject != want.ComposeProject || project.Workdir != want.Workdir ||
		*project.Healthcheck != *want.Healthcheck || project.Runtime != want.Runtime || project.Environment != want.Environment {
		t.Errorf("Parse = %+v\nse esperaba %+v", project, want)
	}
}

// withPre reemplaza la lista vacía de deploy.pre del proyecto Docker por lo
// que se le pase, ya indentado.
func withPre(steps string) string {
	return strings.Replace(dockerYAML, "  pre: []", "  pre:\n"+strings.TrimSuffix(steps, "\n"), 1)
}

func TestParseSpecSteps(t *testing.T) {
	yaml := withPre(`    - service: backend
      command: ["python", "manage.py", "migrate", "--noinput"]
      timeout_seconds: 600
    - service: backend.worker_1
      command: [./scripts/cargar datos.sh, "--con espacios", "ñandú"]`)
	yaml = strings.Replace(yaml, "  post: []", "  post:\n    - service: web\n      command: [curl, -fsS, http://127.0.0.1/warmup]", 1)

	spec, err := ParseSpec("cristales-baez.yaml", []byte(yaml))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if spec.Project.Name != "cristales-baez" {
		t.Errorf("proyecto = %+v", spec.Project)
	}
	if len(spec.Pre) != 2 || len(spec.Post) != 1 {
		t.Fatalf("pasos = %+v / %+v", spec.Pre, spec.Post)
	}
	first := spec.Pre[0]
	if first.Service != "backend" || strings.Join(first.Command, "|") != "python|manage.py|migrate|--noinput" || first.Timeout != 600*time.Second {
		t.Errorf("primer paso = %+v", first)
	}
	// Los argumentos se pasan tal cual: sin separar por espacios ni interpretar.
	if got := spec.Pre[1].Command; len(got) != 3 || got[1] != "--con espacios" || got[2] != "ñandú" {
		t.Errorf("segundo paso = %+v", got)
	}
	if spec.Pre[1].Timeout != DefaultStepTimeout {
		t.Errorf("timeout por defecto = %s", spec.Pre[1].Timeout)
	}
	if spec.Post[0].Service != "web" || spec.Post[0].Command[1] != "-fsS" {
		t.Errorf("paso post = %+v", spec.Post[0])
	}

	// Parse devuelve el mismo proyecto, sin los pasos: los pasos no viajan.
	project, err := Parse("cristales-baez.yaml", []byte(yaml))
	if err != nil || project.Name != spec.Project.Name || project.ComposeProject != spec.Project.ComposeProject {
		t.Errorf("Parse = %+v, %v", project, err)
	}
}

func TestParseSpecWithoutSteps(t *testing.T) {
	for name, content := range map[string]string{
		"listas vacías":  dockerYAML,
		"sin deploy":     strings.Replace(dockerYAML, "deploy:\n  pre: []\n  post: []\n", "", 1),
		"deploy vacío":   strings.Replace(dockerYAML, "deploy:\n  pre: []\n  post: []\n", "deploy:\n", 1),
		"listas nulas":   strings.Replace(dockerYAML, "  pre: []\n  post: []\n", "  pre:\n  post:\n", 1),
		"systemd vacías": systemdYAML + "deploy:\n  pre: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			filename := "cristales-baez.yaml"
			if strings.HasPrefix(content, "name: facturas") {
				filename = "facturas.yaml"
			}
			spec, err := ParseSpec(filename, []byte(content))
			if err != nil {
				t.Fatalf("ParseSpec: %v", err)
			}
			if len(spec.Pre) != 0 || len(spec.Post) != 0 {
				t.Errorf("pasos = %+v / %+v", spec.Pre, spec.Post)
			}
		})
	}
}

func TestParseSystemdWithDefaults(t *testing.T) {
	project, err := Parse("facturas.yml", []byte(systemdYAML+"healthcheck:\n  url: http://127.0.0.1:8000/health\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(project.Units) != 2 || project.ComposeProject != "" {
		t.Errorf("proyecto = %+v", project)
	}
	if project.Healthcheck.ExpectStatus != DefaultExpectStatus || project.Healthcheck.TimeoutSeconds != DefaultTimeoutSeconds {
		t.Errorf("healthcheck sin los valores por defecto: %+v", *project.Healthcheck)
	}
}

// Un proyecto systemd con todo lo que usa el deploy (ADR-019): el usuario, el
// archivo de entorno y los pasos, que no llevan servicio y cuyo programa se
// resuelve a una ruta absoluta.
func TestParseSystemdSpec(t *testing.T) {
	yaml := systemdYAML + `run_as: facturas
env_file: /etc/facturas/env
deploy:
  build:
    - command: [".venv/bin/pip", "install", "-r", "requirements.txt"]
      timeout_seconds: 600
    - command: ["./scripts/estaticos.sh"]
  pre:
    - command: [".venv/bin/python", "manage.py", "migrate", "--noinput"]
  post:
    - command: ["/usr/bin/curl", "-fsS", "http://127.0.0.1:8000/warmup"]
rollback: previous_commit
`
	spec, err := ParseSpec("facturas.yaml", []byte(yaml))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if spec.RunAs != "facturas" || spec.EnvFile != "/etc/facturas/env" {
		t.Errorf("usuario = %q, entorno = %q", spec.RunAs, spec.EnvFile)
	}
	if len(spec.Build) != 2 || len(spec.Pre) != 1 || len(spec.Post) != 1 {
		t.Fatalf("pasos = %d build, %d pre, %d post", len(spec.Build), len(spec.Pre), len(spec.Post))
	}

	pip := spec.Build[0]
	if pip.Exec != "/srv/facturas/.venv/bin/pip" || pip.Service != "" || pip.Timeout != 600*time.Second {
		t.Errorf("build[0] = %+v", pip)
	}
	// Lo que se escribió se conserva, para mostrarlo.
	if strings.Join(pip.Command, " ") != ".venv/bin/pip install -r requirements.txt" {
		t.Errorf("build[0].Command = %q", pip.Command)
	}
	if got := spec.Build[1]; got.Exec != "/srv/facturas/scripts/estaticos.sh" || got.Timeout != DefaultStepTimeout {
		t.Errorf("build[1] = %+v", got)
	}
	if got := spec.Pre[0].Exec; got != "/srv/facturas/.venv/bin/python" {
		t.Errorf("pre[0].Exec = %q", got)
	}
	// Una ruta absoluta queda tal cual.
	if got := spec.Post[0].Exec; got != "/usr/bin/curl" {
		t.Errorf("post[0].Exec = %q", got)
	}

	// Nada de esto viaja al control plane: el proyecto es el mismo con o sin.
	plain, err := Parse("facturas.yaml", []byte(systemdYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if spec.Project.Workdir != plain.Workdir || len(spec.Project.Units) != len(plain.Units) {
		t.Errorf("el proyecto cambió con los pasos: %+v", spec.Project)
	}
}

// Un proyecto systemd que ya estaba declarado, sin usuario ni pasos, sigue
// valiendo: desplegarlo es traer el código y reiniciar.
func TestParseSystemdWithoutStepsNeedsNoUser(t *testing.T) {
	for name, extra := range map[string]string{
		"nada":             "",
		"deploy vacío":     "deploy:\n  build: []\n  pre: []\n  post: []\n",
		"rollback":         "rollback: previous_commit\n",
		"usuario sin paso": "run_as: facturas\nenv_file: /etc/facturas/env\n",
	} {
		t.Run(name, func(t *testing.T) {
			spec, err := ParseSpec("facturas.yaml", []byte(systemdYAML+extra))
			if err != nil {
				t.Fatalf("ParseSpec: %v", err)
			}
			if len(spec.Build)+len(spec.Pre)+len(spec.Post) != 0 {
				t.Errorf("pasos = %+v", spec)
			}
		})
	}
}

// Cada caso rompe el archivo de una forma y verifica que se rechace con un
// error que nombra lo que está mal.
func TestParseRejects(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		yaml     string
		wantErr  string
	}{
		{name: "vacío", filename: "x.yaml", yaml: "", wantErr: "vacío"},
		{name: "no es YAML", filename: "x.yaml", yaml: "name: [a\n", wantErr: "YAML inválido"},
		{name: "campo desconocido", filename: "cristales-baez.yaml", yaml: dockerYAML + "comando: rm -rf /\n", wantErr: "el campo comando no existe"},
		{name: "campo desconocido anidado", filename: "cristales-baez.yaml", yaml: strings.Replace(dockerYAML, "  timeout_seconds: 5\n", "  timeout_seconds: 5\n  headers: {}\n", 1), wantErr: "el campo headers no existe"},
		{name: "campo repetido", filename: "cristales-baez.yaml", yaml: dockerYAML + "branch: otra\n", wantErr: "YAML inválido"},
		{name: "dos documentos", filename: "cristales-baez.yaml", yaml: dockerYAML + "---\nname: otro\n", wantErr: "más de un documento"},
		{name: "nombre distinto del archivo", filename: "otro.yaml", yaml: dockerYAML, wantErr: "igual al nombre del archivo"},
		{name: "sin nombre", filename: "x.yaml", yaml: "environment: production\n", wantErr: "name: falta"},
		{name: "paso como texto de shell", filename: "cristales-baez.yaml", yaml: withPre("    - python manage.py migrate"), wantErr: "un paso {service, command, timeout_seconds}"},
		{name: "pasos como mapa", filename: "cristales-baez.yaml", yaml: strings.Replace(dockerYAML, "  post: []", "  post: {run: x}", 1), wantErr: "una lista de pasos"},
		{name: "paso sin servicio", filename: "cristales-baez.yaml", yaml: withPre("    - command: [ls]"), wantErr: "deploy.pre[0].service: falta"},
		{name: "servicio que parece opción", filename: "cristales-baez.yaml", yaml: withPre("    - service: --privileged\n      command: [ls]"), wantErr: "deploy.pre[0].service"},
		{name: "paso sin comando", filename: "cristales-baez.yaml", yaml: withPre("    - service: backend"), wantErr: "deploy.pre[0].command: falta"},
		{name: "comando como texto", filename: "cristales-baez.yaml", yaml: withPre("    - service: backend\n      command: python manage.py migrate"), wantErr: "YAML inválido"},
		{name: "comando que empieza como opción", filename: "cristales-baez.yaml", yaml: withPre("    - service: backend\n      command: [--entrypoint=sh]"), wantErr: "no puede empezar con -"},
		{name: "argumento vacío", filename: "cristales-baez.yaml", yaml: withPre("    - service: backend\n      command: [ls, \"\"]"), wantErr: "deploy.pre[0].command[1]"},
		{name: "argumento con salto de línea", filename: "cristales-baez.yaml", yaml: withPre("    - service: backend\n      command: [\"ls\\nrm\"]"), wantErr: "caracteres de control"},
		{name: "argumento enorme", filename: "cristales-baez.yaml", yaml: withPre("    - service: backend\n      command: [" + strings.Repeat("x", MaxStepArgLen+1) + "]"), wantErr: "deploy.pre[0].command[0]"},
		{name: "demasiados argumentos", filename: "cristales-baez.yaml", yaml: withPre("    - service: backend\n      command: [" + strings.Repeat("x, ", MaxStepArgs) + "x]"), wantErr: "argumentos y el máximo"},
		{name: "timeout fuera de rango", filename: "cristales-baez.yaml", yaml: withPre("    - service: backend\n      command: [ls]\n      timeout_seconds: 3600"), wantErr: "timeout_seconds"},
		{name: "campo desconocido en un paso", filename: "cristales-baez.yaml", yaml: withPre("    - service: backend\n      command: [ls]\n      shell: true"), wantErr: "el campo shell no existe"},
		{name: "demasiados pasos", filename: "cristales-baez.yaml", yaml: withPre(strings.Repeat("    - service: backend\n      command: [ls]\n", MaxSteps+1)), wantErr: "pasos y el máximo"},
		{name: "paso de systemd con servicio", filename: "facturas.yaml", yaml: systemdYAML + "run_as: facturas\ndeploy:\n  post:\n    - service: web\n      command: [./ls]\n", wantErr: "deploy.post[0].service: es solo para proyectos de Docker"},
		{name: "pasos de systemd sin usuario", filename: "facturas.yaml", yaml: systemdYAML + "deploy:\n  pre:\n    - command: [.venv/bin/python, manage.py, migrate]\n", wantErr: "run_as: falta"},
		{name: "entorno sin usuario", filename: "facturas.yaml", yaml: systemdYAML + "env_file: /etc/facturas/env\n", wantErr: "env_file: necesita run_as"},
		{name: "pasos con root", filename: "facturas.yaml", yaml: systemdYAML + "run_as: root\n", wantErr: "no puede ser root"},
		{name: "pasos con el usuario del agent", filename: "facturas.yaml", yaml: systemdYAML + "run_as: statussv\n", wantErr: "no puede ser root ni statussv"},
		{name: "usuario que parece opción", filename: "facturas.yaml", yaml: systemdYAML + "run_as: \"-u root\"\n", wantErr: "run_as: tiene que ser un nombre de usuario"},
		{name: "usuario numérico", filename: "facturas.yaml", yaml: systemdYAML + "run_as: \"0\"\n", wantErr: "run_as: tiene que ser un nombre de usuario"},
		{name: "entorno relativo", filename: "facturas.yaml", yaml: systemdYAML + "run_as: facturas\nenv_file: .env\n", wantErr: "env_file: tiene que ser la ruta absoluta"},
		{name: "entorno con ..", filename: "facturas.yaml", yaml: systemdYAML + "run_as: facturas\nenv_file: /etc/facturas/../shadow\n", wantErr: "env_file"},
		{name: "programa que se sale del workdir", filename: "facturas.yaml", yaml: systemdYAML + "run_as: facturas\ndeploy:\n  build:\n    - command: [../otro/bin/pip, install]\n", wantErr: "deploy.build[0].command[0]: tiene que ser la ruta del programa"},
		{name: "programa que es el workdir", filename: "facturas.yaml", yaml: systemdYAML + "run_as: facturas\ndeploy:\n  build:\n    - command: [.]\n", wantErr: "deploy.build[0].command[0]"},
		{name: "programa absoluto con ..", filename: "facturas.yaml", yaml: systemdYAML + "run_as: facturas\ndeploy:\n  build:\n    - command: [/srv/facturas/../../bin/sh]\n", wantErr: "deploy.build[0].command[0]"},
		{name: "build con docker", filename: "cristales-baez.yaml", yaml: strings.Replace(dockerYAML, "  pre: []", "  build:\n    - command: [./make.sh]\n  pre: []", 1), wantErr: "deploy.build: es solo para proyectos de systemd"},
		{name: "usuario con docker", filename: "cristales-baez.yaml", yaml: dockerYAML + "run_as: app\n", wantErr: "run_as y env_file: son solo para proyectos de systemd"},
		{name: "rollback de systemd con docker", filename: "cristales-baez.yaml", yaml: strings.Replace(dockerYAML, "previous_image", "previous_commit", 1), wantErr: "rollback: con runtime docker"},
		{name: "rollback inventado", filename: "cristales-baez.yaml", yaml: strings.Replace(dockerYAML, "previous_image", "git_revert", 1), wantErr: "rollback"},
		{name: "rollback con systemd", filename: "facturas.yaml", yaml: systemdYAML + "rollback: previous_image\n", wantErr: "rollback"},
		{name: "tipo equivocado", filename: "cristales-baez.yaml", yaml: strings.Replace(dockerYAML, "expect_status: 200", "expect_status: doscientos", 1), wantErr: "YAML inválido"},
		{name: "workdir relativo", filename: "cristales-baez.yaml", yaml: strings.Replace(dockerYAML, "/srv/cristales-baez", "srv/cristales", 1), wantErr: "workdir"},
		{name: "compose fuera de workdir", filename: "cristales-baez.yaml", yaml: strings.Replace(dockerYAML, "docker-compose.yml", "../../etc/compose.yml", 1), wantErr: "compose_file"},
		{name: "systemd sin units", filename: "facturas.yaml", yaml: strings.Split(systemdYAML, "units:")[0], wantErr: "units"},
		{name: "healthcheck con credenciales", filename: "cristales-baez.yaml", yaml: strings.Replace(dockerYAML, "https://cristales", "https://juan:clave@cristales", 1), wantErr: "usuario ni contraseña"},
		{name: "archivo enorme", filename: "cristales-baez.yaml", yaml: dockerYAML + "#" + strings.Repeat("x", MaxFileBytes), wantErr: "pesa"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.filename, []byte(tt.yaml))
			if err == nil {
				t.Fatal("se aceptó un archivo inválido")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, se esperaba que dijera %q", err, tt.wantErr)
			}
			// El error viaja al control plane: tiene que entrar en el contrato.
			if err := (proto.InvalidProject{File: tt.filename, Error: err.Error()}).Validate(); err != nil {
				t.Errorf("el error no entra en el contrato: %v", err)
			}
		})
	}
}

// Ningún error de un archivo trae sus valores: viajan al control plane.
func TestParseErrorsNeverIncludeValues(t *testing.T) {
	const secret = "s3cr3t0"
	files := map[string]string{
		"usuario":      systemdYAML + "run_as: " + secret + "!\n",
		"entorno file": systemdYAML + "run_as: facturas\nenv_file: " + secret + "/env\n",
		"programa":     systemdYAML + "run_as: facturas\ndeploy:\n  build:\n    - command: [../" + secret + "/pip]\n",
		"tipo":         strings.Replace(dockerYAML, "expect_status: 200", "expect_status: "+secret, 1),
		"url":          strings.Replace(dockerYAML, "https://cristales", "https://juan:"+secret+"@cristales", 1),
		"repo":         strings.Replace(dockerYAML, "github.com/extendsystem/cristales-baez", secret+" ; rm", 1),
		"rama":         strings.Replace(dockerYAML, "branch: main", "branch: "+secret+"..x", 1),
		"pasos":        strings.Replace(dockerYAML, "  pre: []", "  pre: ["+secret+"]", 1),
		"argumento":    withPre("    - service: backend\n      command: [\"ls\\n" + secret + "\"]"),
		"servicio":     withPre("    - service: " + secret + "!\n      command: [ls]"),
		"entorno":      strings.Replace(dockerYAML, "environment: production", "environment: "+secret, 1),
		"lista rota":   strings.Replace(systemdYAML, "  - facturas-worker.service", "  - {"+secret+": 1}", 1),
		"no es un map": secret + "\n",
	}
	for name, content := range files {
		t.Run(name, func(t *testing.T) {
			filename := "cristales-baez.yaml"
			if strings.HasPrefix(content, "name: facturas") {
				filename = "facturas.yaml"
			}
			_, err := Parse(filename, []byte(content))
			if err == nil {
				t.Fatal("se aceptó un archivo inválido")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("el error muestra un valor del archivo: %v", err)
			}
		})
	}
}

func TestNameFromFile(t *testing.T) {
	for filename, want := range map[string]string{"a.yaml": "a", "b.yml": "b", "/etc/status-sv/projects.d/c.yaml": "c"} {
		if got, err := NameFromFile(filename); err != nil || got != want {
			t.Errorf("NameFromFile(%q) = %q, %v", filename, got, err)
		}
	}
	for _, filename := range []string{"README.md", "x.yaml.bak", "x.json", "yaml"} {
		if _, err := NameFromFile(filename); !errors.Is(err, ErrNotProjectFile) {
			t.Errorf("NameFromFile(%q) = %v, se esperaba ErrNotProjectFile", filename, err)
		}
	}
}

func TestDefaultComposeProject(t *testing.T) {
	for workdir, want := range map[string]string{
		"/srv/cristales-baez": "cristales-baez",
		"/srv/Mi.App":         "miapp",
		"/opt/_privado":       "privado",
		"/srv/app_v2":         "app_v2",
	} {
		if got := DefaultComposeProject(workdir); got != want {
			t.Errorf("DefaultComposeProject(%q) = %q, se esperaba %q", workdir, got, want)
		}
	}
}
