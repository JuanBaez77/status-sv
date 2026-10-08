package proposals

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

func systemdRequest() Request {
	return Request{
		Name: "svdemo", Environment: "staging", Repo: "github.com/JuanBaez77/status-sv-demo", Branch: "main",
		Runtime: "systemd", Command: []string{".venv/bin/gunicorn", "app:app", "--bind", "127.0.0.1:18097"},
		RequiredEnv: []string{"SECRET_KEY"},
		Site:        &Site{Domain: "svdemo.extendsystem.com", Port: 18097},
		Healthcheck: &Healthcheck{URL: "http://127.0.0.1:18097/health"},
		Build: []Step{
			{Command: []string{"/usr/bin/python3", "-m", "venv", ".venv"}},
			{Command: []string{".venv/bin/pip", "install", "-r", "requirements.txt"}, TimeoutSeconds: 600},
		},
	}
}

func dockerRequest() Request {
	return Request{
		Name: "tienda", Environment: "production", Repo: "github.com/a/tienda", Branch: "main",
		Runtime: "docker", ComposeFile: "compose.yml", RequiredEnv: []string{"DATABASE_URL"},
		Pre: []Step{{Service: "web", Command: []string{"bin/migrate"}}},
	}
}

// Lo que la propuesta no elige lo completa Build: el workdir, el usuario, el
// archivo de entorno y la unidad.
func TestBuildFillsWhatIsNotChosen(t *testing.T) {
	data, spec, err := Build(systemdRequest())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, want := range []string{
		"workdir: /srv/svdemo\n", "run_as: svdemo\n", "env_file: /etc/status-sv/env/svdemo.env\n",
		"required_env: [SECRET_KEY]\n", "  domain: svdemo.extendsystem.com\n",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("falta %q en\n%s", want, data)
		}
	}
	if spec.Project.Workdir != "/srv/svdemo" || len(spec.Project.Units) != 1 || spec.Project.Units[0] != "svdemo.service" {
		t.Errorf("spec = %+v", spec.Project)
	}

	data, spec, err = Build(dockerRequest())
	if err != nil {
		t.Fatalf("Build docker: %v", err)
	}
	if strings.Contains(string(data), "run_as") || strings.Contains(string(data), "env_file") || spec.Service != nil {
		t.Errorf("un proyecto Docker no lleva usuario ni unidad:\n%s", data)
	}
	if spec.Project.Workdir != "/srv/tienda" || len(spec.Pre) != 1 {
		t.Errorf("spec = %+v", spec)
	}
}

// Un comando de Docker no se cuela en un proyecto Docker: el runtime decide
// qué campos se escriben.
func TestBuildIgnoresFieldsOfTheOtherRuntime(t *testing.T) {
	req := dockerRequest()
	req.Command = []string{"bin/server"}
	data, _, err := Build(req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(string(data), "\nservice:") {
		t.Errorf("se escribió un servicio en un proyecto Docker:\n%s", data)
	}

	req = systemdRequest()
	req.ComposeFile = "compose.yml"
	data, _, err = Build(req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(string(data), "compose_file") {
		t.Errorf("se escribió un compose en un proyecto systemd:\n%s", data)
	}
}

func TestBuildRejects(t *testing.T) {
	cases := map[string]struct {
		edit func(*Request)
		want string
	}{
		"nombre con mayúsculas":  {func(r *Request) { r.Name = "SvDemo" }, "name"},
		"nombre con ruta":        {func(r *Request) { r.Name = "../etc" }, "name"},
		"runtime desconocido":    {func(r *Request) { r.Runtime = "podman" }, "runtime"},
		"entorno desconocido":    {func(r *Request) { r.Environment = "dev" }, "environment"},
		"repo con credenciales":  {func(r *Request) { r.Repo = "https://user:pass@github.com/a/b" }, "repo"},
		"variable en minúsculas": {func(r *Request) { r.RequiredEnv = []string{"secret"} }, "required_env"},
		"dominio comodín":        {func(r *Request) { r.Site.Domain = "*.extendsystem.com" }, "site.domain"},
		"puerto privilegiado":    {func(r *Request) { r.Site.Port = 80 }, "site.port"},
		"servicio afuera":        {func(r *Request) { r.Command = []string{"../../bin/sh"} }, "service"},
		"sin servicio":           {func(r *Request) { r.Command = nil }, "servicio"},
		"demasiadas variables": {func(r *Request) {
			r.RequiredEnv = nil
			for i := range proto.MaxRequiredEnv + 1 {
				r.RequiredEnv = append(r.RequiredEnv, "VAR_"+strings.Repeat("A", i+1))
			}
		}, "required_env"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			req := systemdRequest()
			c.edit(&req)
			_, _, err := Build(req)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Build = %v, se esperaba ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("el error no nombra %q: %v", c.want, err)
			}
		})
	}
}

// La declaración no puede pasar el tope del contrato: el agent la rechazaría.
func TestBuildRejectsOversized(t *testing.T) {
	req := systemdRequest()
	long := strings.Repeat("x", 200)
	for range 200 {
		req.Build = append(req.Build, Step{Command: []string{"bin/tool", long}})
	}
	if _, _, err := Build(req); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Build = %v, se esperaba ErrInvalid", err)
	}
}

func TestMakePreview(t *testing.T) {
	p, err := MakePreview(systemdRequest())
	if err != nil {
		t.Fatalf("MakePreview: %v", err)
	}
	if p.Command != "sudo statussv-agent project accept svdemo" {
		t.Errorf("comando = %q", p.Command)
	}
	if !strings.Contains(p.Unit, `ExecStart="/srv/svdemo/.venv/bin/gunicorn" "app:app" "--bind" "127.0.0.1:18097"`) ||
		!strings.Contains(p.Unit, "User=svdemo") || !strings.Contains(p.Unit, "EnvironmentFile=/etc/status-sv/env/svdemo.env") {
		t.Errorf("unidad =\n%s", p.Unit)
	}
	if !strings.Contains(p.Site, "server_name svdemo.extendsystem.com;") || !strings.Contains(p.Site, "proxy_pass http://127.0.0.1:18097;") {
		t.Errorf("sitio =\n%s", p.Site)
	}
	if !strings.HasPrefix(p.YAML, "name: svdemo\n") {
		t.Errorf("yaml =\n%s", p.YAML)
	}

	// Un proyecto Docker sin sitio no tiene unidad ni sitio que mostrar.
	p, err = MakePreview(dockerRequest())
	if err != nil {
		t.Fatalf("MakePreview docker: %v", err)
	}
	if p.Unit != "" || p.Site != "" {
		t.Errorf("vista previa Docker = %+v", p)
	}

	if _, err := MakePreview(Request{Name: "x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("MakePreview de algo inválido = %v", err)
	}
}

// La vista previa de una propuesta guardada sale de lo guardado, igual que la
// del pedido.
func TestPreviewOfStored(t *testing.T) {
	data, _, err := Build(systemdRequest())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	stored, err := PreviewOf(store.ProjectProposal{Name: "svdemo", Spec: string(data)})
	if err != nil {
		t.Fatalf("PreviewOf: %v", err)
	}
	fresh, err := MakePreview(systemdRequest())
	if err != nil {
		t.Fatalf("MakePreview: %v", err)
	}
	if stored != fresh {
		t.Errorf("guardada = %+v\npedida = %+v", stored, fresh)
	}
}

func TestMessages(t *testing.T) {
	data, _, err := Build(dockerRequest())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	id := uuid.Must(uuid.NewV7())
	expires := time.Date(2026, 10, 16, 12, 0, 0, 0, time.FixedZone("ART", -3*60*60))
	m := Message(store.ProjectProposal{ID: id, Name: "tienda", Spec: string(data), SpecHash: proto.SpecHash(data), ExpiresAt: expires})
	if err := m.Validate(); err != nil {
		t.Fatalf("el mensaje no vale: %v", err)
	}
	if m.Action != proto.ProposalPut || m.YAML != string(data) || m.ExpiresAt.Location() != time.UTC || !m.ExpiresAt.Equal(expires) {
		t.Errorf("mensaje = %+v", m)
	}
	w := WithdrawMessage(id, "tienda")
	if err := w.Validate(); err != nil {
		t.Fatalf("el retiro no vale: %v", err)
	}
	if w.Action != proto.ProposalWithdraw || w.YAML != "" {
		t.Errorf("retiro = %+v", w)
	}
}
