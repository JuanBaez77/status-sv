package accept

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/JuanBaez77/status-sv/agent/internal/projects"
	"github.com/JuanBaez77/status-sv/internal/projectspec"
	"github.com/JuanBaez77/status-sv/internal/templates"
)

// rendered es lo que generan las plantillas de fábrica para svdemo.
func rendered(t *testing.T) (unit, http, tls []byte) {
	t.Helper()
	spec, err := projectspec.ParseSpec("svdemo.yaml", []byte(svdemoYAML))
	if err != nil {
		t.Fatal(err)
	}
	set := templates.Default()
	if unit, err = set.Service(spec); err != nil {
		t.Fatal(err)
	}
	if http, err = set.Site(spec, templates.SiteHTTP); err != nil {
		t.Fatal(err)
	}
	if tls, err = set.Site(spec, templates.SiteTLS); err != nil {
		t.Fatal(err)
	}
	return unit, http, tls
}

func (e *env) unitFile() string { return e.a.onDisk("/etc/systemd/system/svdemo.service") }
func (e *env) siteFile() string {
	return e.a.onDisk("/etc/nginx/sites-available/statussv-svdemo.conf")
}
func (e *env) siteLink() string { return e.a.onDisk("/etc/nginx/sites-enabled/statussv-svdemo.conf") }

// withNginx arma un nginx de mentira: sus directorios y un certbot que deja
// el certificado en su lugar.
func (e *env) withNginx(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("el sitio se habilita con un enlace simbólico, que en Windows pide permisos especiales")
	}
	for _, dir := range []string{"/etc/nginx/sites-available", "/etc/nginx/sites-enabled"} {
		if err := os.MkdirAll(e.a.onDisk(dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	e.sys.live = e.a.onDisk("/etc/letsencrypt/live")
}

func TestAcceptWritesUnit(t *testing.T) {
	e := newEnv(t)
	e.propose(t, "svdemo", svdemoYAML)
	if err := e.a.Accept(t.Context(), "svdemo", Options{Yes: true}); err != nil {
		t.Fatalf("Accept: %v\n%s", err, e.out)
	}
	unit, _, _ := rendered(t)
	got, err := os.ReadFile(e.unitFile())
	if err != nil || string(got) != string(unit) {
		t.Errorf("unidad = %q, %v", got, err)
	}
	if e.sys.daemonReloads != 1 {
		t.Errorf("daemon-reload = %d", e.sys.daemonReloads)
	}
	// La unidad se muestra entera antes de confirmar.
	if !strings.Contains(e.out.String(), string(unit)) {
		t.Errorf("la salida no muestra la unidad:\n%s", e.out)
	}
}

// Que el programa todavía no exista es lo esperable: lo crea el primer
// deploy. Cualquier otra queja de systemd frena el alta y saca la unidad.
func TestUnitVerify(t *testing.T) {
	e := newEnv(t)
	e.propose(t, "svdemo", svdemoYAML)
	e.sys.verifyOut = "otra.service: algo\nsvdemo.service: Command /srv/svdemo/.venv/bin/gunicorn is not executable: No such file or directory\n"
	if err := e.a.Accept(t.Context(), "svdemo", Options{Yes: true}); err != nil {
		t.Fatalf("Accept con el programa faltante: %v", err)
	}

	e = newEnv(t)
	e.propose(t, "svdemo", svdemoYAML)
	e.sys.verifyOut = "svdemo.service:14: Unknown key name 'Foo' in section 'Service', ignoring.\n"
	err := e.a.Accept(t.Context(), "svdemo", Options{Yes: true})
	if err == nil || !strings.Contains(err.Error(), "Unknown key name") {
		t.Fatalf("Accept = %v", err)
	}
	if _, err := os.Stat(e.unitFile()); !os.IsNotExist(err) {
		t.Errorf("la unidad quedó: %v", err)
	}
	if e.sys.daemonReloads != 0 || e.sys.restarts != 0 {
		t.Error("siguió después de una unidad rechazada")
	}
}

// Una unidad con ese nombre que no es la que se generaría no se pisa.
func TestUnitExistsDifferent(t *testing.T) {
	e := newEnv(t)
	e.propose(t, "svdemo", svdemoYAML)
	if err := os.WriteFile(e.unitFile(), []byte("[Service]\nExecStart=/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := e.a.Accept(t.Context(), "svdemo", Options{Yes: true})
	if err == nil || !strings.Contains(err.Error(), "no se pisa") {
		t.Fatalf("Accept = %v", err)
	}
	if len(e.sys.created) != 0 || e.sys.keygens != 0 {
		t.Error("se cambió algo antes de frenar")
	}
}

func TestAcceptMakesSite(t *testing.T) {
	e := newEnv(t)
	e.withNginx(t)
	e.propose(t, "svdemo", svdemoYAML)
	if err := e.a.Accept(t.Context(), "svdemo", Options{Yes: true, Email: "juan@example.com"}); err != nil {
		t.Fatalf("Accept: %v\n%s", err, e.out)
	}
	_, _, tls := rendered(t)
	got, err := os.ReadFile(e.siteFile())
	if err != nil || string(got) != string(tls) {
		t.Errorf("sitio = %q, %v", got, err)
	}
	if target, err := os.Readlink(e.siteLink()); err != nil || target != e.siteFile() {
		t.Errorf("enlace = %q, %v", target, err)
	}
	// Primero de prueba, después de verdad; con el desafío servido desde el
	// directorio de las plantillas y el nombre fijo del certificado.
	if len(e.sys.certbot) != 2 || !slices.Contains(e.sys.certbot[0], "--dry-run") || slices.Contains(e.sys.certbot[1], "--dry-run") {
		t.Fatalf("certbot = %q", e.sys.certbot)
	}
	args := strings.Join(e.sys.certbot[1], " ")
	for _, want := range []string{"certonly --webroot -w /var/www/statussv-acme", "--cert-name statussv-svdemo", "-d svdemo.extendsystem.com", "--email juan@example.com", "--deploy-hook systemctl reload nginx"} {
		if !strings.Contains(args, want) {
			t.Errorf("certbot %s no tiene %q", args, want)
		}
	}
	// nginx -t antes de cada recarga: el sitio del desafío y el completo.
	if e.sys.nginxTests != 2 || e.sys.reloads != 2 {
		t.Errorf("nginx -t = %d, recargas = %d", e.sys.nginxTests, e.sys.reloads)
	}
}

// Con el certificado ya pedido, se escribe el sitio completo y nada más.
func TestSiteWithCertificate(t *testing.T) {
	e := newEnv(t)
	e.withNginx(t)
	cert := filepath.Join(e.sys.live, "statussv-svdemo")
	if err := os.MkdirAll(cert, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cert, "fullchain.pem"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.propose(t, "svdemo", svdemoYAML)
	if err := e.a.Accept(t.Context(), "svdemo", Options{Yes: true}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if len(e.sys.certbot) != 0 || e.sys.nginxTests != 1 {
		t.Errorf("certbot = %q, nginx -t = %d", e.sys.certbot, e.sys.nginxTests)
	}
}

// Lo que impide el sitio no frena el alta: se explica y se reintenta con
// project site. No se escribe nada de nginx.
func TestSiteSkips(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, e *env)
		want  string
	}{
		{name: "otro sitio con el dominio", setup: func(t *testing.T, e *env) {
			other := e.a.onDisk("/etc/nginx/sites-enabled/svdemo-viejo")
			if err := os.WriteFile(other, []byte("server {\n    server_name www.algo.com svdemo.extendsystem.com;\n}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "svdemo-viejo ya atiende"},
		{name: "sin DNS", setup: func(_ *testing.T, e *env) { e.sys.unresolved = true }, want: "todavía no resuelve"},
		{name: "sin certbot", setup: func(_ *testing.T, e *env) { e.sys.missing = map[string]bool{"certbot": true} }, want: "no está certbot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.withNginx(t)
			tt.setup(t, e)
			e.propose(t, "svdemo", svdemoYAML)
			if err := e.a.Accept(t.Context(), "svdemo", Options{Yes: true}); err != nil {
				t.Fatalf("Accept: %v", err)
			}
			if !strings.Contains(e.out.String(), tt.want) || !strings.Contains(e.out.String(), "project site svdemo") {
				t.Errorf("la salida no lo explica:\n%s", e.out)
			}
			if _, err := os.Lstat(e.siteFile()); !os.IsNotExist(err) {
				t.Errorf("se escribió el sitio: %v", err)
			}
			if e.sys.reloads != 0 || len(e.sys.certbot) != 0 {
				t.Errorf("se tocó nginx: recargas %d, certbot %q", e.sys.reloads, e.sys.certbot)
			}
			// El resto del alta siguió.
			if e.sys.restarts != 1 {
				t.Errorf("reinicios = %d", e.sys.restarts)
			}
		})
	}
}

// Si nginx -t no acepta el sitio completo, se saca todo lo de esta corrida y
// se recarga solo con lo que pasa la prueba.
func TestSiteRevertsWhenNginxRejects(t *testing.T) {
	e := newEnv(t)
	e.withNginx(t)
	e.sys.nginxErrs = []error{nil, errors.New("[emerg] algo"), nil}
	e.propose(t, "svdemo", svdemoYAML)
	if err := e.a.Accept(t.Context(), "svdemo", Options{Yes: true}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	for _, path := range []string{e.siteFile(), e.siteLink()} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s quedó: %v", path, err)
		}
	}
	// Una recarga con el sitio del desafío y otra después de deshacer; nunca
	// con lo que nginx -t rechazó.
	if e.sys.nginxTests != 3 || e.sys.reloads != 2 {
		t.Errorf("nginx -t = %d, recargas = %d", e.sys.nginxTests, e.sys.reloads)
	}
	if !strings.Contains(e.out.String(), "se deshizo") {
		t.Errorf("no lo explica:\n%s", e.out)
	}
}

// Si nginx -t sigue fallando después de deshacer, no se recarga nginx.
func TestSiteDoesNotReloadABrokenNginx(t *testing.T) {
	e := newEnv(t)
	e.withNginx(t)
	broken := errors.New("[emerg] de otro sitio")
	e.sys.nginxErrs = []error{broken, broken}
	e.propose(t, "svdemo", svdemoYAML)
	if err := e.a.Accept(t.Context(), "svdemo", Options{Yes: true}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if e.sys.reloads != 0 {
		t.Errorf("se recargó nginx %d veces con una configuración rota", e.sys.reloads)
	}
}

func TestSiteRevertsWhenCertbotFails(t *testing.T) {
	e := newEnv(t)
	e.withNginx(t)
	e.sys.certbotErr = errors.New("Challenge failed")
	e.propose(t, "svdemo", svdemoYAML)
	if err := e.a.Accept(t.Context(), "svdemo", Options{Yes: true}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := os.Lstat(e.siteFile()); !os.IsNotExist(err) {
		t.Errorf("el sitio del desafío quedó: %v", err)
	}
	if len(e.sys.certbot) != 1 {
		t.Errorf("después de fallar la prueba se pidió el de verdad: %q", e.sys.certbot)
	}
}

// project site arma el sitio cuando el DNS está listo.
func TestSiteCommand(t *testing.T) {
	e := newEnv(t)
	e.withNginx(t)
	e.sys.unresolved = true
	e.propose(t, "svdemo", svdemoYAML)
	if err := e.a.Accept(t.Context(), "svdemo", Options{Yes: true}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	e.sys.unresolved = false
	e.a.LoadSpec = projects.ReadSpec
	if err := e.a.Site(t.Context(), "svdemo", ""); err != nil {
		t.Fatalf("Site: %v\n%s", err, e.out)
	}
	if _, err := os.Stat(e.siteFile()); err != nil {
		t.Errorf("no quedó el sitio: %v", err)
	}
	// Otra vez no hace nada: ya está.
	before := e.sys.nginxTests
	if err := e.a.Site(t.Context(), "svdemo", ""); err != nil || e.sys.nginxTests != before {
		t.Errorf("segunda vez: %v, nginx -t %d → %d", err, before, e.sys.nginxTests)
	}
}
