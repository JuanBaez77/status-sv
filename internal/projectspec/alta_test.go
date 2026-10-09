package projectspec

import (
	"strings"
	"testing"

	"github.com/JuanBaez77/status-sv/proto"
)

// svdemoYAML es un proyecto systemd como lo arma una propuesta (ADR-022): con
// la unidad generada, el sitio y las variables requeridas.
const svdemoYAML = `name: svdemo
environment: staging
repo: github.com/JuanBaez77/status-sv-demo
branch: main
runtime: systemd
workdir: /srv/svdemo
run_as: svdemo
env_file: /etc/status-sv/env/svdemo.env
required_env: [SECRET_KEY, DATABASE_URL]
service:
  command: [".venv/bin/gunicorn", "app:app", "--bind", "127.0.0.1:18097"]
site:
  domain: svdemo.extendsystem.com
  port: 18097
deploy:
  build:
    - command: [/usr/bin/python3, -m, venv, .venv]
    - command: [.venv/bin/pip, install, -r, requirements.txt]
`

// dockerSiteYAML es un proyecto Docker propuesto, con su sitio y sus variables.
const dockerSiteYAML = `name: svdemo-docker
environment: staging
repo: github.com/JuanBaez77/status-sv-demo
branch: main
runtime: docker
compose_file: docker-compose.yml
workdir: /srv/svdemo-docker
required_env: [SECRET_KEY]
site:
  domain: docker.svdemo.extendsystem.com
  port: 18098
`

func TestParseAltaSystemd(t *testing.T) {
	spec, err := ParseSpec("svdemo.yaml", []byte(svdemoYAML))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	// Sin units, el proyecto tiene la unidad que se genera.
	if units := spec.Project.Units; len(units) != 1 || units[0] != "svdemo.service" {
		t.Errorf("units = %q", units)
	}
	if spec.Service == nil || spec.Service.Exec != "/srv/svdemo/.venv/bin/gunicorn" {
		t.Fatalf("service = %+v", spec.Service)
	}
	if strings.Join(spec.Service.Command, " ") != ".venv/bin/gunicorn app:app --bind 127.0.0.1:18097" {
		t.Errorf("service.Command = %q", spec.Service.Command)
	}
	if spec.Site == nil || *spec.Site != (Site{Domain: "svdemo.extendsystem.com", Port: 18097}) {
		t.Errorf("site = %+v", spec.Site)
	}
	// El sitio viaja con el proyecto; las variables y el servicio, no.
	if site := spec.Project.Site; site == nil || site.Domain != "svdemo.extendsystem.com" || site.Port != 18097 {
		t.Errorf("project.Site = %+v", site)
	}
	if err := spec.Project.Validate(); err != nil {
		t.Errorf("el proyecto no entra en el contrato: %v", err)
	}
	if strings.Join(spec.RequiredEnv, ",") != "SECRET_KEY,DATABASE_URL" {
		t.Errorf("required_env = %q", spec.RequiredEnv)
	}
	if err := CheckProposal(spec); err != nil {
		t.Errorf("CheckProposal: %v", err)
	}

	// Declarar la unidad es opcional, pero si se declara es esa.
	withUnits := strings.Replace(svdemoYAML, "runtime: systemd\n", "runtime: systemd\nunits: [svdemo.service]\n", 1)
	if _, err := ParseSpec("svdemo.yaml", []byte(withUnits)); err != nil {
		t.Errorf("con units explícitas: %v", err)
	}
}

func TestParseAltaDocker(t *testing.T) {
	spec, err := ParseSpec("svdemo-docker.yaml", []byte(dockerSiteYAML))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	// Con Docker los valores van al .env del workdir: no hace falta env_file.
	if spec.Site == nil || spec.Site.Port != 18098 || len(spec.RequiredEnv) != 1 || spec.Service != nil {
		t.Errorf("spec = %+v", spec)
	}
	if err := CheckProposal(spec); err != nil {
		t.Errorf("CheckProposal: %v", err)
	}
}

// Un proyecto declarado antes del alta, sin los campos nuevos, sigue igual.
func TestParseWithoutAltaFields(t *testing.T) {
	for name, content := range map[string]string{"facturas.yaml": systemdYAML, "cristales-baez.yaml": dockerYAML} {
		spec, err := ParseSpec(name, []byte(content))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if spec.Service != nil || spec.Site != nil || spec.RequiredEnv != nil {
			t.Errorf("%s: %+v", name, spec)
		}
	}
}

func TestParseAltaRejects(t *testing.T) {
	replace := func(old, repl string) string { return strings.Replace(svdemoYAML, old, repl, 1) }
	dockerReplace := func(old, repl string) string { return strings.Replace(dockerSiteYAML, old, repl, 1) }
	tests := []struct {
		name    string
		file    string
		yaml    string
		wantErr string
	}{
		{name: "servicio con docker", file: "svdemo-docker.yaml", yaml: dockerSiteYAML + "service:\n  command: [./run]\n", wantErr: "service: es solo para proyectos de systemd"},
		{name: "servicio con otra unidad", yaml: replace("runtime: systemd\n", "runtime: systemd\nunits: [gunicorn.service]\n"), wantErr: "units: con service"},
		{name: "servicio con dos unidades", yaml: replace("runtime: systemd\n", "runtime: systemd\nunits: [svdemo.service, otro.service]\n"), wantErr: "units: con service"},
		{name: "servicio sin usuario", yaml: replace("run_as: svdemo\nenv_file: /etc/status-sv/env/svdemo.env\nrequired_env: [SECRET_KEY, DATABASE_URL]\n", ""), wantErr: "run_as: falta"},
		{name: "servicio sin comando", yaml: replace(`  command: [".venv/bin/gunicorn", "app:app", "--bind", "127.0.0.1:18097"]`, "  command: []"), wantErr: "service.command: falta"},
		{name: "servicio como texto", yaml: replace(`  command: [".venv/bin/gunicorn", "app:app", "--bind", "127.0.0.1:18097"]`, "  command: gunicorn app:app"), wantErr: "YAML inválido"},
		{name: "servicio que se sale del workdir", yaml: replace(`".venv/bin/gunicorn"`, `"../otro/gunicorn"`), wantErr: "service.command[0]: tiene que ser la ruta"},
		{name: "servicio con salto de línea", yaml: replace(`"app:app"`, `"app\nrm"`), wantErr: "service.command[1]"},
		{name: "servicio sin lista", yaml: replace(`service:
  command: [".venv/bin/gunicorn", "app:app", "--bind", "127.0.0.1:18097"]`, "service: gunicorn app:app"), wantErr: "un servicio {command}"},
		{name: "sitio como texto", yaml: replace("site:\n  domain: svdemo.extendsystem.com\n  port: 18097", "site: svdemo.extendsystem.com"), wantErr: "un sitio {domain, port}"},
		{name: "campo desconocido en el servicio", yaml: replace("service:\n", "service:\n  user: root\n"), wantErr: "el campo user no existe"},
		{name: "variable en minúsculas", yaml: replace("SECRET_KEY,", "secret_key,"), wantErr: "required_env[0]"},
		{name: "variable que empieza con número", yaml: replace("SECRET_KEY,", "1KEY,"), wantErr: "required_env[0]"},
		{name: "variable con =", yaml: replace("SECRET_KEY,", "\"A=B\","), wantErr: "required_env[0]"},
		{name: "variable repetida", yaml: replace("DATABASE_URL]", "SECRET_KEY]"), wantErr: "required_env[1]: está repetido"},
		{name: "demasiadas variables", yaml: replace("[SECRET_KEY, DATABASE_URL]", "["+strings.Repeat("A, ", proto.MaxRequiredEnv)+"B]"), wantErr: "nombres y el máximo"},
		{name: "variables de systemd sin archivo", yaml: replace("env_file: /etc/status-sv/env/svdemo.env\n", ""), wantErr: "required_env: con systemd necesita env_file"},
		{name: "dominio en mayúsculas", yaml: replace("svdemo.extendsystem.com", "SvDemo.extendsystem.com"), wantErr: "site.domain"},
		{name: "dominio comodín", yaml: replace("svdemo.extendsystem.com", "\"*.extendsystem.com\""), wantErr: "site.domain"},
		{name: "dominio de una etiqueta", yaml: replace("svdemo.extendsystem.com", "localhost"), wantErr: "site.domain"},
		{name: "dominio con puerto", yaml: replace("svdemo.extendsystem.com", "\"svdemo.extendsystem.com:443\""), wantErr: "site.domain"},
		{name: "dominio con espacio y directiva", yaml: replace("svdemo.extendsystem.com", "\"a.com; include /etc/shadow\""), wantErr: "site.domain"},
		{name: "dirección IP", yaml: replace("svdemo.extendsystem.com", "\"10.0.0.1\""), wantErr: "site.domain"},
		{name: "puerto reservado", yaml: replace("port: 18097", "port: 80"), wantErr: "site.port"},
		{name: "puerto imposible", yaml: replace("port: 18097", "port: 70000"), wantErr: "site.port"},
		{name: "sitio sin dominio", file: "svdemo-docker.yaml", yaml: dockerReplace("  domain: docker.svdemo.extendsystem.com\n", ""), wantErr: "site.domain"},
		{name: "campo desconocido en el sitio", file: "svdemo-docker.yaml", yaml: dockerReplace("site:\n", "site:\n  root: /etc\n"), wantErr: "el campo root no existe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := tt.file
			if file == "" {
				file = "svdemo.yaml"
			}
			_, err := ParseSpec(file, []byte(tt.yaml))
			if err == nil {
				t.Fatal("se aceptó un archivo inválido")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, se esperaba que dijera %q", err, tt.wantErr)
			}
			if err := (proto.InvalidProject{File: file, Error: err.Error()}).Validate(); err != nil {
				t.Errorf("el error no entra en el contrato: %v", err)
			}
		})
	}
}

// Lo que una propuesta no elige (ADR-022): cada caso es un project.yaml válido
// que no vale como propuesta.
func TestCheckProposalRejects(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		yaml    string
		wantErr string
	}{
		{name: "workdir propio", yaml: strings.ReplaceAll(svdemoYAML, "workdir: /srv/svdemo", "workdir: /opt/svdemo"), wantErr: "workdir"},
		{name: "workdir de otro proyecto con docker", file: "svdemo-docker.yaml", yaml: strings.Replace(dockerSiteYAML, "/srv/svdemo-docker", "/srv/otro", 1), wantErr: "workdir"},
		{name: "otro usuario", yaml: strings.Replace(svdemoYAML, "run_as: svdemo", "run_as: postgres", 1), wantErr: "run_as"},
		{name: "otro archivo de entorno", yaml: strings.Replace(svdemoYAML, "/etc/status-sv/env/svdemo.env", "/etc/shadow", 1), wantErr: "env_file"},
		{name: "sin unidad generada", yaml: strings.Replace(strings.Split(svdemoYAML, "service:")[0], "runtime: systemd\n", "runtime: systemd\nunits: [gunicorn.service]\n", 1), wantErr: "service: falta"},
		{name: "nombre que empieza con número", file: "1demo.yaml", yaml: strings.NewReplacer("svdemo", "1demo", "run_as: svdemo", "run_as: otro").Replace(svdemoYAML), wantErr: "name: un proyecto systemd propuesto"},
		{name: "nombre de más de 32", file: strings.Repeat("a", 33) + ".yaml", yaml: strings.NewReplacer("run_as: svdemo", "run_as: otro", "svdemo", strings.Repeat("a", 33)).Replace(svdemoYAML), wantErr: "name: un proyecto systemd propuesto"},
		{name: "repo https con token", yaml: strings.Replace(svdemoYAML, "repo: github.com/", "repo: https://x-access-token:ghp_abc@github.com/", 1), wantErr: "repo: una propuesta no lleva"},
		{name: "repo https con usuario", yaml: strings.Replace(svdemoYAML, "repo: github.com/", "repo: https://juan@github.com/", 1), wantErr: "repo: una propuesta no lleva"},
		{name: "repo ssh con contraseña", yaml: strings.Replace(svdemoYAML, "repo: github.com/", "repo: ssh://git:clave@github.com/", 1), wantErr: "repo: una propuesta no lleva"},
		{name: "repo scp con contraseña", yaml: strings.Replace(svdemoYAML, "repo: github.com/JuanBaez77/", "repo: git:clave@github.com:JuanBaez77/", 1), wantErr: "repo: una propuesta no lleva"},
		{name: "repo por http", yaml: strings.Replace(svdemoYAML, "repo: github.com/", "repo: http://github.com/", 1), wantErr: "repo: en una propuesta, solo https o ssh"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := tt.file
			if file == "" {
				file = "svdemo.yaml"
			}
			spec, err := ParseSpec(file, []byte(tt.yaml))
			if err != nil {
				t.Fatalf("ParseSpec: %v (el caso tiene que ser un project.yaml válido)", err)
			}
			err = CheckProposal(spec)
			if err == nil {
				t.Fatal("se aceptó como propuesta")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, se esperaba que dijera %q", err, tt.wantErr)
			}
		})
	}
}

// Los errores de los campos nuevos tampoco traen sus valores.
func TestAltaErrorsNeverIncludeValues(t *testing.T) {
	const secret = "s3cr3t0"
	files := map[string]string{
		"dominio":  strings.Replace(svdemoYAML, "svdemo.extendsystem.com", secret+".EXAMPLE.com", 1),
		"variable": strings.Replace(svdemoYAML, "SECRET_KEY,", secret+",", 1),
		"servicio": strings.Replace(svdemoYAML, `".venv/bin/gunicorn"`, `"../`+secret+`/gunicorn"`, 1),
		"puerto":   strings.Replace(svdemoYAML, "port: 18097", "port: "+secret, 1),
	}
	for name, content := range files {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSpec("svdemo.yaml", []byte(content))
			if err == nil {
				t.Fatal("se aceptó un archivo inválido")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("el error muestra un valor del archivo: %v", err)
			}
		})
	}
}

// Las formas de repo que usa un alta de verdad pasan.
func TestCheckProposalRepoAccepts(t *testing.T) {
	for _, repo := range []string{
		"github.com/JuanBaez77/status-sv-demo",
		"git@github.com:JuanBaez77/status-sv-demo.git",
		"https://github.com/JuanBaez77/status-sv-demo.git",
		"ssh://git@github.com/JuanBaez77/status-sv-demo.git",
	} {
		if err := checkProposalRepo(repo); err != nil {
			t.Errorf("%s: %v", repo, err)
		}
	}
}
