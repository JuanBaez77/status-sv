package accept

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/agent/internal/proposals"
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
deploy:
  build:
    - command: [/usr/bin/python3, -m, venv, .venv]
`

const dockerYAML = `name: tienda
environment: production
repo: github.com/JuanBaez77/tienda
branch: main
runtime: docker
compose_file: docker-compose.yml
workdir: /srv/tienda
`

var now = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// fakeSystem anota lo que se le pide.
type fakeSystem struct {
	root       bool
	users      map[string]bool
	created    []string
	chowns     map[string]string
	keygens    int
	restarts   int
	checks     []string
	checkErr   error
	restartErr error
	onRestart  func()

	// La unidad y el sitio.
	missing       map[string]bool // programas que no están
	unresolved    bool
	verifyOut     string
	nginxErrs     []error // lo que contesta cada nginx -t, en orden
	nginxTests    int
	reloads       int
	daemonReloads int
	certbot       [][]string
	certbotErr    error
	// live es dónde certbot de mentira deja el certificado.
	live string
	// acme es de dónde sirve nginx el desafío; con notServing, no lo sirve
	// (otro sitio atiende el dominio).
	acme       string
	notServing bool
	fetches    int
}

func newFakeSystem() *fakeSystem {
	return &fakeSystem{root: true, users: map[string]bool{"root": true, AgentUser: true}, chowns: map[string]string{}}
}

func (f *fakeSystem) IsRoot() bool                         { return f.root }
func (f *fakeSystem) UserExists(name string) (bool, error) { return f.users[name], nil }
func (f *fakeSystem) CreateUser(name, _ string) error {
	f.users[name] = true
	f.created = append(f.created, name)
	return nil
}

func (f *fakeSystem) Chown(path, owner, group string) error {
	f.chowns[path] = owner + ":" + group
	return nil
}

func (f *fakeSystem) Keygen(path, comment string) error {
	f.keygens++
	if err := os.WriteFile(path, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(path+".pub", []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample "+comment+"\n"), 0o600)
}

func (f *fakeSystem) Hostname() string { return "vps-prueba" }

// Owner devuelve el usuario que corre la prueba: cambiarle el dueño a un
// archivo propio no necesita root.
func (f *fakeSystem) Owner(string, string) (int, int, error) { return os.Getuid(), os.Getgid(), nil }

func (f *fakeSystem) RestartAgent(context.Context) error {
	f.restarts++
	if f.restartErr != nil {
		return f.restartErr
	}
	if f.onRestart != nil {
		f.onRestart()
	}
	return nil
}

func (f *fakeSystem) DeployCheck(_ context.Context, project string) error {
	f.checks = append(f.checks, project)
	return f.checkErr
}

// env es un servidor de mentira: los directorios del agent en un temporal,
// con el deploy prendido.
type env struct {
	a   *Accepter
	sys *fakeSystem
	out *bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	paths := Paths{
		State:        filepath.Join(base, "var", "lib", "status-sv"),
		Projects:     filepath.Join(base, "etc", "status-sv", "projects.d"),
		Keys:         filepath.Join(base, "etc", "status-sv", "keys"),
		Dropin:       filepath.Join(base, "etc", "systemd", "20-deploy.conf"),
		HelperSocket: filepath.Join(base, "etc", "systemd", "statussv-unit-helper.socket"),
		Root:         filepath.Join(base, "root"),
	}
	for _, dir := range []string{paths.State, paths.Projects, paths.Keys, filepath.Dir(paths.Dropin), filepath.Join(paths.Root, "etc", "systemd", "system")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{paths.Dropin, paths.HelperSocket} {
		if err := os.WriteFile(file, []byte("[Service]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sys := newFakeSystem()
	out := &bytes.Buffer{}
	return &env{
		a:   &Accepter{Paths: paths, Sys: sys, In: strings.NewReader("svdemo\n\n"), Out: out, Now: func() time.Time { return now }},
		sys: sys,
		out: out,
	}
}

// propose guarda una propuesta como lo hace el agent.
func (e *env) propose(t *testing.T, name, yaml string) {
	t.Helper()
	store := proposals.NewStore(filepath.Join(e.a.Paths.State, proposals.DirName), func(string) bool { return false })
	ack := store.Handle(proto.ProjectProposal{
		ProposalID: uuid.New(), Name: name, Action: proto.ProposalPut, YAML: yaml,
		ExpiresAt: time.Now().Add(7 * 24 * time.Hour),
	})
	if ack.Result != proto.ProposalStored {
		t.Fatalf("guardar la propuesta: %+v", ack)
	}
}

func TestAcceptSystemd(t *testing.T) {
	e := newEnv(t)
	e.propose(t, "svdemo", svdemoYAML)

	if err := e.a.Accept(t.Context(), "svdemo", Options{}); err != nil {
		t.Fatalf("Accept: %v\n%s", err, e.out)
	}

	// La declaración es exactamente la propuesta, de root y del grupo del agent.
	declared, err := os.ReadFile(filepath.Join(e.a.Paths.Projects, "svdemo.yaml"))
	if err != nil || string(declared) != svdemoYAML {
		t.Errorf("declaración = %q, %v", declared, err)
	}
	if len(e.sys.created) != 1 || e.sys.created[0] != "svdemo" {
		t.Errorf("usuarios creados = %v", e.sys.created)
	}
	workdir := filepath.Join(e.a.Paths.Root, "srv", "svdemo")
	info, err := os.Stat(workdir)
	if err != nil || !info.IsDir() {
		t.Fatalf("workdir: %v", err)
	}
	if got := e.sys.chowns[workdir]; got != "svdemo:"+AgentUser {
		t.Errorf("dueño del workdir = %q", got)
	}
	if runtime.GOOS == "linux" && info.Mode()&os.ModeSetgid == 0 {
		t.Errorf("el workdir no tiene setgid: %v", info.Mode())
	}
	key := filepath.Join(e.a.Paths.Keys, "svdemo")
	if e.sys.keygens != 1 || e.sys.chowns[key] != AgentUser+":"+AgentUser {
		t.Errorf("keygens = %d, dueño de la llave = %q", e.sys.keygens, e.sys.chowns[key])
	}
	// ssh rechaza una llave que otros pueden leer, y deploy-check lo revisa.
	if info, err := os.Stat(key); err != nil {
		t.Errorf("la llave: %v", err)
	} else if runtime.GOOS == "linux" && info.Mode().Perm() != 0o400 {
		t.Errorf("la llave quedó %v", info.Mode())
	}

	// El drop-in suma el workdir y el agent se reinicia una vez.
	dropin, _ := os.ReadFile(e.a.Paths.Dropin)
	if !strings.Contains(string(dropin), "ReadWritePaths=-/srv/svdemo\n") || e.sys.restarts != 1 {
		t.Errorf("drop-in = %q, reinicios = %d", dropin, e.sys.restarts)
	}
	if len(e.sys.checks) != 1 || e.sys.checks[0] != "svdemo" {
		t.Errorf("deploy-check = %v", e.sys.checks)
	}

	out := e.out.String()
	for _, want := range []string{
		svdemoYAML,                              // la declaración entera
		"build: /usr/bin/python3 -m venv .venv", // los comandos que van a correr
		"servicio: .venv/bin/gunicorn app:app --bind 127.0.0.1:18097",
		"ssh-ed25519 AAAA", // la llave para GitHub
		"gh repo deploy-key add statussv-svdemo.pub --repo JuanBaez77/status-sv-demo",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("la salida no muestra %q:\n%s", want, out)
		}
	}

	// Al terminar, la propuesta se borra: aceptarla otra vez ya no se puede.
	if _, err := os.Stat(filepath.Join(e.a.Paths.State, proposals.DirName, "svdemo.yaml")); !os.IsNotExist(err) {
		t.Errorf("la propuesta sigue guardada: %v", err)
	}
	e.a.In, e.a.in = strings.NewReader("svdemo\n"), nil
	if err := e.a.Accept(t.Context(), "svdemo", Options{}); err == nil || !strings.Contains(err.Error(), "no hay una propuesta") {
		t.Errorf("otra vez después de terminar: %v", err)
	}
}

// Si el alta se corta en el medio, correrla de nuevo sigue donde quedó: no
// rehace el usuario ni la llave.
func TestAcceptResumes(t *testing.T) {
	e := newEnv(t)
	e.propose(t, "svdemo", svdemoYAML)
	e.sys.restartErr = errors.New("systemctl: falló")
	if err := e.a.Accept(t.Context(), "svdemo", Options{Yes: true}); err == nil {
		t.Fatal("Accept no falló con el reinicio roto")
	}
	e.sys.restartErr = nil
	if err := e.a.Accept(t.Context(), "svdemo", Options{Yes: true}); err != nil {
		t.Fatalf("segunda vez: %v\n%s", err, e.out)
	}
	if len(e.sys.created) != 1 || e.sys.keygens != 1 || e.sys.restarts != 2 {
		t.Errorf("usuarios %v, llaves %d, reinicios %d", e.sys.created, e.sys.keygens, e.sys.restarts)
	}
	if !strings.Contains(e.out.String(), "el proyecto ya está declarado igual a la propuesta") {
		t.Errorf("la segunda vez no dice que ya estaba declarado:\n%s", e.out)
	}
}

func TestAcceptDocker(t *testing.T) {
	e := newEnv(t)
	e.propose(t, "tienda", dockerYAML)
	e.a.In = strings.NewReader("tienda\n\n")
	if err := e.a.Accept(t.Context(), "tienda", Options{NoDeployKey: true}); err != nil {
		t.Fatalf("Accept: %v\n%s", err, e.out)
	}
	if len(e.sys.created) != 0 {
		t.Errorf("un proyecto Docker no crea usuario: %v", e.sys.created)
	}
	if got := e.sys.chowns[filepath.Join(e.a.Paths.Root, "srv", "tienda")]; got != "root:"+AgentUser {
		t.Errorf("dueño del workdir = %q", got)
	}
	if e.sys.keygens != 0 {
		t.Error("con --no-deploy-key se generó una llave")
	}
}

func TestAcceptRefuses(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, e *env)
		wantErr string
	}{
		{name: "sin root", setup: func(_ *testing.T, e *env) { e.sys.root = false }, wantErr: "sudo"},
		{name: "deploy apagado", setup: func(t *testing.T, e *env) {
			if err := os.Remove(e.a.Paths.Dropin); err != nil {
				t.Fatal(err)
			}
		}, wantErr: "apagado"},
		{name: "sin propuesta", setup: func(*testing.T, *env) {}, wantErr: "no hay una propuesta"},
		{name: "vencida", setup: func(t *testing.T, e *env) {
			e.propose(t, "svdemo", svdemoYAML)
			e.a.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
		}, wantErr: "venció"},
		{name: "declarado distinto", setup: func(t *testing.T, e *env) {
			e.propose(t, "svdemo", svdemoYAML)
			if err := os.WriteFile(filepath.Join(e.a.Paths.Projects, "svdemo.yaml"), []byte("name: svdemo\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, wantErr: "no se pisa"},
		{name: "deploy en curso", setup: func(t *testing.T, e *env) {
			e.propose(t, "svdemo", svdemoYAML)
			writeJournal(t, e.a.Paths.State, "tienda", proto.DeployRunning)
		}, wantErr: "deploy en curso (tienda)"},
		{name: "no confirmó", setup: func(t *testing.T, e *env) {
			e.propose(t, "svdemo", svdemoYAML)
			e.a.In = strings.NewReader("otro\n")
		}, wantErr: ErrAborted.Error()},
		{name: "propuesta tocada", setup: func(t *testing.T, e *env) {
			e.propose(t, "svdemo", svdemoYAML)
			path := filepath.Join(e.a.Paths.State, proposals.DirName, "svdemo.yaml")
			if err := os.WriteFile(path, []byte(svdemoYAML+"# otro paso\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, wantErr: "no coincide"},
		{
			// Un agent comprometido puede escribir una propuesta con su ficha
			// bien armada: el comando la valida de nuevo y no la acepta.
			name: "agent comprometido", setup: func(t *testing.T, e *env) {
				evil := strings.ReplaceAll(svdemoYAML, "run_as: svdemo", "run_as: postgres")
				writeRawProposal(t, e.a.Paths.State, "svdemo", evil)
			}, wantErr: "run_as",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			tt.setup(t, e)
			err := e.a.Accept(t.Context(), "svdemo", Options{})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Accept = %v, se esperaba un error que dijera %q", err, tt.wantErr)
			}
			// Nada se tocó.
			if len(e.sys.created) != 0 || e.sys.keygens != 0 || e.sys.restarts != 0 {
				t.Errorf("se cambió algo: usuarios %v, llaves %d, reinicios %d", e.sys.created, e.sys.keygens, e.sys.restarts)
			}
			if _, err := os.Stat(filepath.Join(e.a.Paths.Root, "srv", "svdemo")); err == nil {
				t.Error("se creó el workdir")
			}
		})
	}
}

// Si empieza un deploy mientras se confirmaba, no se reinicia el agent.
func TestAcceptChecksDeploysAgainBeforeRestart(t *testing.T) {
	e := newEnv(t)
	e.propose(t, "svdemo", svdemoYAML)
	e.a.In = &startsDeploy{state: e.a.Paths.State, t: t, input: strings.NewReader("svdemo\n")}
	err := e.a.Accept(t.Context(), "svdemo", Options{})
	if err == nil || !strings.Contains(err.Error(), "deploy en curso") {
		t.Fatalf("Accept = %v", err)
	}
	if e.sys.restarts != 0 {
		t.Error("se reinició el agent con un deploy en curso")
	}
}

// startsDeploy es una terminal que, mientras la persona escribe, arranca un
// deploy.
type startsDeploy struct {
	state string
	t     *testing.T
	input *strings.Reader
	done  bool
}

func (s *startsDeploy) Read(p []byte) (int, error) {
	if !s.done {
		writeJournal(s.t, s.state, "tienda", proto.DeployRunning)
		s.done = true
	}
	return s.input.Read(p)
}

func writeJournal(t *testing.T, stateDir, project string, status proto.DeployStatus) {
	t.Helper()
	dir := filepath.Join(stateDir, "deploys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := uuid.Must(uuid.NewV7())
	raw, err := json.Marshal(map[string]any{"event": proto.DeployEvent{DeployID: id, Seq: 1, Project: project, Status: status}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id.String()+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeRawProposal escribe una propuesta como lo haría un agent comprometido:
// con la ficha bien armada, pero sin pasar por el almacén.
func writeRawProposal(t *testing.T, stateDir, name, yaml string) {
	t.Helper()
	dir := filepath.Join(stateDir, proposals.DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := proposals.Meta{ProposalID: uuid.New(), Name: name, Hash: proto.SpecHash([]byte(yaml)), ExpiresAt: now.Add(time.Hour), ReceivedAt: now}
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	for file, data := range map[string][]byte{name + ".yaml": []byte(yaml), name + ".json": raw} {
		if err := os.WriteFile(filepath.Join(dir, file), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeployCheckFailureIsNotAnError(t *testing.T) {
	e := newEnv(t)
	e.propose(t, "svdemo", svdemoYAML)
	e.sys.checkErr = errors.New("el repo no contesta")
	if err := e.a.Accept(t.Context(), "svdemo", Options{Yes: true}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if !strings.Contains(e.out.String(), "deploy-check encontró problemas") {
		t.Errorf("no dice cómo revisar de nuevo:\n%s", e.out)
	}
}

func TestGithubRepo(t *testing.T) {
	for repo, want := range map[string]string{
		"github.com/JuanBaez77/status-sv-demo":           "JuanBaez77/status-sv-demo",
		"git@github.com:extendsystem/facturas.git":       "extendsystem/facturas",
		"https://github.com/extendsystem/facturas.git":   "extendsystem/facturas",
		"ssh://git@github.com/extendsystem/facturas.git": "extendsystem/facturas",
	} {
		if got, ok := githubRepo(repo); !ok || got != want {
			t.Errorf("githubRepo(%q) = %q, %v", repo, got, ok)
		}
	}
	for _, repo := range []string{"gitlab.com/a/b", "github.com/a", "github.com/a/b/c"} {
		if got, ok := githubRepo(repo); ok {
			t.Errorf("githubRepo(%q) = %q", repo, got)
		}
	}
}

func (f *fakeSystem) DaemonReload(context.Context) error { f.daemonReloads++; return nil }

func (f *fakeSystem) VerifyUnit(context.Context, string) (string, error) { return f.verifyOut, nil }

func (f *fakeSystem) Has(program string) bool { return !f.missing[program] }

func (f *fakeSystem) Resolves(string) bool { return !f.unresolved }

func (f *fakeSystem) NginxTest(context.Context) error {
	f.nginxTests++
	if len(f.nginxErrs) == 0 {
		return nil
	}
	err := f.nginxErrs[0]
	f.nginxErrs = f.nginxErrs[1:]
	return err
}

func (f *fakeSystem) NginxReload(context.Context) error { f.reloads++; return nil }

// Certbot anota los argumentos y, si no es de prueba, deja el certificado
// donde lo dejaría el de verdad.
func (f *fakeSystem) Certbot(_ context.Context, args []string) error {
	f.certbot = append(f.certbot, args)
	if f.certbotErr != nil {
		return f.certbotErr
	}
	for i, arg := range args {
		if arg == "--dry-run" {
			return nil
		}
		if arg == "--cert-name" && f.live != "" {
			dir := filepath.Join(f.live, args[i+1])
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "fullchain.pem"), []byte("certificado"), 0o600)
		}
	}
	return nil
}

// Fetch hace de nginx: sirve el desafío desde su directorio.
func (f *fakeSystem) Fetch(_ context.Context, _ string, path string) (string, error) {
	f.fetches++
	if f.notServing || f.acme == "" || !strings.HasPrefix(path, "/.well-known/acme-challenge/") {
		return "", errors.New("contestó 404")
	}
	data, err := os.ReadFile(filepath.Join(f.acme, filepath.FromSlash(path)))
	if err != nil {
		return "", errors.New("contestó 404")
	}
	return string(data), nil
}
