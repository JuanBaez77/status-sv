package templates

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Estas pruebas le pasan lo generado a las herramientas de verdad:
// systemd-analyze verify y nginx -t. STATUSSV_TEST_TOOLS=1 las prende (el CI
// lo hace), y entonces que falte una herramienta es un error, no un salto.
func tool(t *testing.T, name string) string {
	t.Helper()
	if os.Getenv("STATUSSV_TEST_TOOLS") != "1" {
		t.Skip("STATUSSV_TEST_TOOLS=1 prende las pruebas con systemd-analyze y nginx")
	}
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("falta %s: %v", name, err)
	}
	return path
}

// La unidad generada pasa systemd-analyze verify, con argumentos que systemd
// interpretaría si no estuvieran escapados.
func TestServiceVerifiesWithSystemd(t *testing.T) {
	analyze := tool(t, "systemd-analyze")

	// El programa tiene que existir y ser ejecutable: verify lo revisa.
	workdir := filepath.Join(t.TempDir(), "svdemo")
	program := filepath.Join(workdir, ".venv", "bin", "gunicorn")
	if err := os.MkdirAll(filepath.Dir(program), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // ejecutable de prueba
		t.Fatal(err)
	}

	spec := svdemo(t)
	spec.Project.Workdir = workdir
	spec.Service.Exec = program
	spec.Service.Command = []string{".venv/bin/gunicorn", "app:app", "--bind", "127.0.0.1:18097", "--name", "mi app", "100%", "$HOME", `"x"`}
	unit, err := Default().Service(spec)
	if err != nil {
		t.Fatalf("Service: %v", err)
	}
	file := filepath.Join(t.TempDir(), "svdemo.service")
	if err := os.WriteFile(file, unit, 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(analyze, "verify", "--man=no", file).CombinedOutput() //nolint:gosec // herramienta de prueba
	// verify también se queja de unidades ajenas del sistema; lo que importa
	// es que no diga nada de esta.
	if err != nil || strings.Contains(string(out), "svdemo.service") {
		t.Errorf("systemd-analyze verify: %v\n%s\n--- unidad ---\n%s", err, out, unit)
	}

	// systemd lee los argumentos tal como se escribieron.
	if !strings.Contains(string(unit), `"mi app" "100%%" "$$HOME" "\"x\""`) {
		t.Errorf("escapado inesperado:\n%s", unit)
	}
}

// Los dos sitios generados pasan nginx -t, con un certificado de prueba.
func TestSiteValidatesWithNginx(t *testing.T) {
	nginx := tool(t, "nginx")

	dir := t.TempDir()
	set := Default()
	set.ACMERoot = filepath.Join(dir, "acme")
	set.LiveDir = filepath.Join(dir, "live")
	certDir := filepath.Join(set.LiveDir, CertName("svdemo"))
	writeSelfSigned(t, certDir)

	spec := svdemo(t)
	for _, phase := range []SitePhase{SiteHTTP, SiteTLS} {
		site, err := set.Site(spec, phase)
		if err != nil {
			t.Fatalf("Site: %v", err)
		}
		// Dos proyectos en el mismo nginx: el map de cada uno no choca.
		other := spec
		otherSite := *spec.Site
		otherSite.Domain, otherSite.Port = "otro.extendsystem.com", 18098
		other.Site = &otherSite
		other.Project.Name = "otro-demo"
		writeSelfSigned(t, filepath.Join(set.LiveDir, CertName("otro-demo")))
		second, err := set.Site(other, phase)
		if err != nil {
			t.Fatalf("Site: %v", err)
		}

		sites := filepath.Join(dir, "sites")
		if err := os.MkdirAll(sites, 0o750); err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string][]byte{"svdemo.conf": site, "otro.conf": second} {
			if err := os.WriteFile(filepath.Join(sites, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}

		// Lo mínimo para que nginx -t corra sin root: logs y temporales en
		// el directorio de la prueba.
		conf := filepath.Join(dir, "nginx.conf")
		main := strings.NewReplacer("DIR", dir).Replace(`error_log stderr;
pid DIR/nginx.pid;
events {}
http {
    access_log off;
    client_body_temp_path DIR/body;
    proxy_temp_path DIR/proxy;
    fastcgi_temp_path DIR/fastcgi;
    uwsgi_temp_path DIR/uwsgi;
    scgi_temp_path DIR/scgi;
    include DIR/sites/*.conf;
}
`)
		if err := os.WriteFile(conf, []byte(main), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(nginx, "-t", "-p", dir, "-c", conf).CombinedOutput() //nolint:gosec // herramienta de prueba
		if err != nil {
			t.Errorf("nginx -t (fase %d): %v\n%s\n--- sitio ---\n%s", phase, err, out, site)
		}
	}
}

// writeSelfSigned deja en dir un certificado autofirmado con los nombres que
// usa certbot.
func writeSelfSigned(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "svdemo.extendsystem.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"svdemo.extendsystem.com"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"fullchain.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		"privkey.pem":   pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
