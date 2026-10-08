package projectspec

import (
	"strings"
	"testing"
)

func svdemoDeclaration() Declaration {
	return Declaration{
		Name: "svdemo", Environment: "staging", Repo: "github.com/JuanBaez77/status-sv-demo", Branch: "main",
		Runtime: "systemd", Workdir: ProposalWorkdir("svdemo"), RunAs: "svdemo", EnvFile: ProposalEnvFile("svdemo"),
		RequiredEnv: []string{"SECRET_KEY"},
		Service:     &DeclaredService{Command: []string{".venv/bin/gunicorn", "app:app", "--bind", "127.0.0.1:18097"}},
		Site:        &DeclaredSite{Domain: "svdemo.extendsystem.com", Port: 18097},
		Healthcheck: &DeclaredHealthcheck{URL: "http://127.0.0.1:18097/health"},
		Deploy: &DeclaredSteps{Build: []DeclaredStep{
			{Command: []string{"/usr/bin/python3", "-m", "venv", ".venv"}},
			{Command: []string{".venv/bin/pip", "install", "-r", "requirements.txt"}, TimeoutSeconds: 600},
		}},
	}
}

func TestEncode(t *testing.T) {
	data, spec, err := Encode(svdemoDeclaration())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := `name: svdemo
environment: staging
repo: github.com/JuanBaez77/status-sv-demo
branch: main
runtime: systemd
workdir: /srv/svdemo
run_as: svdemo
env_file: /etc/status-sv/env/svdemo.env
required_env: [SECRET_KEY]
service:
  command: [.venv/bin/gunicorn, 'app:app', --bind, '127.0.0.1:18097']
site:
  domain: svdemo.extendsystem.com
  port: 18097
healthcheck:
  url: http://127.0.0.1:18097/health
deploy:
  build:
    - command: [/usr/bin/python3, -m, venv, .venv]
    - command: [.venv/bin/pip, install, -r, requirements.txt]
      timeout_seconds: 600
`
	if string(data) != want {
		t.Errorf("Encode =\n%s\nse esperaba\n%s", data, want)
	}
	if err := CheckProposal(spec); err != nil {
		t.Errorf("CheckProposal: %v", err)
	}
	if spec.Service.Exec != "/srv/svdemo/.venv/bin/gunicorn" || len(spec.Build) != 2 {
		t.Errorf("spec = %+v", spec)
	}
}

// Lo que Encode escribe pasa por el parser estricto: una declaración que no
// vale no sale.
func TestEncodeRejects(t *testing.T) {
	d := svdemoDeclaration()
	d.Site.Domain = "*.extendsystem.com"
	if _, _, err := Encode(d); err == nil || !strings.Contains(err.Error(), "site.domain") {
		t.Errorf("Encode = %v", err)
	}
	d = svdemoDeclaration()
	d.Service.Command = []string{"../afuera"}
	if _, _, err := Encode(d); err == nil {
		t.Error("se escribió un servicio que se sale del workdir")
	}
	// Un argumento con caracteres que YAML interpreta queda citado, y se lee
	// igual.
	d = svdemoDeclaration()
	d.Service.Command = []string{".venv/bin/app", "#no-es-comentario", "a: b", "[x]", "yes", "1e3", "'citado'"}
	data, spec, err := Encode(d)
	if err != nil {
		t.Fatalf("Encode: %v\n%s", err, data)
	}
	if got := strings.Join(spec.Service.Command, "|"); got != ".venv/bin/app|#no-es-comentario|a: b|[x]|yes|1e3|'citado'" {
		t.Errorf("se leyó %q\n%s", got, data)
	}
}
