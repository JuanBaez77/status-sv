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
		{name: "pasos con systemd", filename: "facturas.yaml", yaml: systemdYAML + "deploy:\n  post:\n    - service: web\n      command: [ls]\n", wantErr: "solo para runtime docker"},
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
