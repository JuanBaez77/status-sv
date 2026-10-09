package accept

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/JuanBaez77/status-sv/internal/projectspec"
	"github.com/JuanBaez77/status-sv/internal/templates"
	"github.com/JuanBaez77/status-sv/proto"
)

// La unidad y el sitio salen de las plantillas (ADR-023). Los escribe este
// comando, que corre una persona como root, y no el ayudante ni el agent: el
// ayudante sigue sin red, y nada que llegue del control plane toca nginx.

// Dónde viven las unidades y los sitios en el servidor.
const (
	unitsDir = "/etc/systemd/system"
	nginxDir = "/etc/nginx"
)

// errSkip es un sitio que no se escribe por algo que la persona tiene que
// resolver (el DNS, otro sitio con el mismo dominio): se explica, y se
// reintenta con project site.
type errSkip struct{ reason string }

func (e errSkip) Error() string { return e.reason }

// templateSet devuelve las plantillas: las del servidor si las hay y son de
// root, o las de fábrica.
func (a *Accepter) templateSet() (*templates.Set, error) {
	if a.Templates == nil {
		return templates.Default(), nil
	}
	return a.Templates()
}

// unitStep es el paso que escribe la unidad generada de un proyecto systemd
// que declara service.
func (a *Accepter) unitStep(acc *acceptance) (action, bool, error) {
	if acc.spec.Service == nil {
		return action{}, false, nil
	}
	shown := path.Join(unitsDir, templates.ServiceUnitFile(acc.name))
	file := a.onDisk(shown)
	existing, err := os.ReadFile(file) //nolint:gosec // nombre validado
	switch {
	case err == nil && bytes.Equal(existing, acc.unit):
		return action{what: fmt.Sprintf("la unidad %s ya está igual", shown), done: true}, true, nil
	case err == nil:
		return action{}, false, fmt.Errorf("ya existe %s y es distinta de la que se generaría: no se pisa", shown)
	case !errors.Is(err, fs.ErrNotExist):
		return action{}, false, err
	}
	return action{
		what: fmt.Sprintf("escribir la unidad %s desde la plantilla (%s), sin arrancarla", shown, acc.set.ServiceOrigin),
		apply: func(ctx context.Context) error {
			if err := a.writeRootFile(file, acc.unit, 0o644, "root"); err != nil {
				return err
			}
			if err := a.verifyUnit(ctx, file, acc); err != nil {
				_ = os.Remove(file)
				return err
			}
			return a.Sys.DaemonReload(ctx)
		},
	}, true, nil
}

// verifyUnit le pasa la unidad a systemd-analyze verify. Lo único que se deja
// pasar es que el programa todavía no exista: lo crea el primer deploy.
func (a *Accepter) verifyUnit(ctx context.Context, path string, acc *acceptance) error {
	out, err := a.Sys.VerifyUnit(ctx, path)
	if err != nil {
		return fmt.Errorf("systemd-analyze verify: %w", err)
	}
	var problems []string
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, filepath.Base(path)) {
			continue // habla de otras unidades del servidor
		}
		if strings.Contains(line, acc.spec.Service.Exec) && strings.Contains(line, "is not executable") {
			continue
		}
		problems = append(problems, line)
	}
	if len(problems) > 0 {
		return fmt.Errorf("systemd no acepta la unidad generada (¿la plantilla del servidor?):\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// siteStep es el paso que arma el sitio y pide el certificado. Nunca frena el
// alta: si algo falla, deshace lo suyo, lo explica y se reintenta con
// project site.
func (a *Accepter) siteStep(acc *acceptance) (action, bool) {
	if acc.spec.Site == nil {
		return action{}, false
	}
	return action{
		what: fmt.Sprintf("armar el sitio de nginx de %s y pedir su certificado a Let's Encrypt", acc.spec.Site.Domain),
		apply: func(ctx context.Context) error {
			if err := a.makeSite(ctx, acc.spec, acc.set, acc.opts.Email); err != nil {
				a.printf("aviso: el sitio no quedó: %v\n", err)
				a.printf("El resto del alta sigue. Cuando esté resuelto: sudo statussv-agent project site %s\n", acc.name)
			}
			return nil
		},
	}, true
}

// makeSite arma el sitio en dos tiempos (ADR-023, punto 5): primero solo el
// desafío de Let's Encrypt, después el certificado, y al final el sitio
// completo. Si algo falla, deja nginx como estaba y nunca recarga una
// configuración que no pasa nginx -t.
func (a *Accepter) makeSite(ctx context.Context, spec projectspec.Spec, set *templates.Set, email string) error {
	name, domain := spec.Project.Name, spec.Site.Domain
	for _, program := range []string{"nginx", "certbot"} {
		if !a.Sys.Has(program) {
			return errSkip{fmt.Sprintf("no está %s en este servidor: el sitio y el certificado se hacen a mano", program)}
		}
	}
	available := a.onDisk(path.Join(nginxDir, "sites-available"))
	enabled := a.onDisk(path.Join(nginxDir, "sites-enabled"))
	for _, dir := range []string{available, enabled} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return errSkip{fmt.Sprintf("no existe %s: este nginx no usa sites-available y sites-enabled", dir)}
		}
	}
	fileName := templates.SiteFileName(name)
	if other := otherSiteFor(enabled, domain, fileName); other != "" {
		return errSkip{fmt.Sprintf("el sitio %s ya atiende %s: no se pisa", other, domain)}
	}
	if !a.Sys.Resolves(domain) {
		return errSkip{fmt.Sprintf("%s todavía no resuelve: creá el registro DNS que apunte a este servidor", domain)}
	}

	tls, err := set.Site(spec, templates.SiteTLS)
	if err != nil {
		return err
	}
	site := siteFiles{file: filepath.Join(available, fileName), link: filepath.Join(enabled, fileName)}
	site.remember()
	certDir := a.onDisk(path.Join(set.LiveDir, templates.CertName(name)))
	haveCert := fileExists(filepath.Join(certDir, "fullchain.pem"))
	if haveCert && bytes.Equal(site.before, tls) && site.linked {
		a.printf("el sitio de %s ya está, con su certificado\n", domain)
		return nil
	}

	if !haveCert {
		http, err := set.Site(spec, templates.SiteHTTP)
		if err != nil {
			return err
		}
		acme := a.onDisk(set.ACMERoot)
		if err := os.MkdirAll(acme, 0o755); err != nil { //nolint:gosec // nginx sirve el desafío desde acá
			return err
		}
		if err := a.writeSite(ctx, site, http); err != nil {
			return err
		}
		if err := a.waitChallenge(ctx, domain, acme); err != nil {
			a.revertSite(ctx, site)
			return err
		}
		args := []string{
			"certonly", "--webroot", "-w", set.ACMERoot, "--cert-name", templates.CertName(name), "-d", domain,
			"--non-interactive", "--agree-tos", "--keep-until-expiring", "--deploy-hook", "systemctl reload nginx",
		}
		if email != "" {
			args = append(args, "--email", email)
		}
		// Primero de prueba: Let's Encrypt limita los certificados repetidos
		// y los intentos fallidos.
		for _, try := range [][]string{append(args[:len(args):len(args)], "--dry-run"), args} {
			if err := a.Sys.Certbot(ctx, try); err != nil {
				a.revertSite(ctx, site)
				return fmt.Errorf("certbot no pudo pedir el certificado (con el proxy de Cloudflare, el modo SSL tiene que ser Full): %w", err)
			}
		}
	}
	if err := a.writeSite(ctx, site, tls); err != nil {
		return err
	}
	a.printf("sitio listo: https://%s (contesta el proyecto desde su primer deploy)\n", domain)
	return nil
}

// challengeWait es cuánto se espera a que nginx sirva el desafío de Let's
// Encrypt: systemctl reload vuelve antes de que nginx tome la configuración
// nueva.
var challengeWait = 10 * time.Second

// waitChallenge deja un archivo de prueba donde certbot deja el desafío y
// espera a que nginx lo sirva para el dominio. Así no se gasta un intento de
// Let's Encrypt, que los limita, si nginx todavía no tomó el sitio o si lo
// atiende otro.
func (a *Accepter) waitChallenge(ctx context.Context, domain, acme string) error {
	dir := filepath.Join(acme, ".well-known", "acme-challenge")
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // nginx sirve el desafío desde acá
		return err
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	token := "statussv-" + hex.EncodeToString(random)
	probe := filepath.Join(dir, token)
	if err := os.WriteFile(probe, []byte(token), 0o644); err != nil { //nolint:gosec // nginx lo tiene que leer
		return err
	}
	defer func() { _ = os.Remove(probe) }()

	deadline := time.Now().Add(challengeWait)
	for {
		got, err := a.Sys.Fetch(ctx, domain, "/.well-known/acme-challenge/"+token)
		if err == nil && got == token {
			return nil
		}
		if err == nil {
			err = errors.New("contestó otra cosa")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nginx no sirve el desafío de Let's Encrypt de %s (%w): revisá que ningún otro sitio atienda ese dominio", domain, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// siteFiles es el archivo del sitio y su enlace, y cómo estaban antes de esta
// corrida, para volver a eso si algo falla.
type siteFiles struct {
	file, link string
	before     []byte
	existed    bool
	linked     bool
}

func (s *siteFiles) remember() {
	data, err := os.ReadFile(s.file)
	s.existed = err == nil
	s.before = data
	_, err = os.Lstat(s.link)
	s.linked = err == nil
}

// writeSite escribe el sitio, lo habilita, lo prueba con nginx -t y recién
// entonces recarga nginx. Si no pasa, vuelve a como estaba.
func (a *Accepter) writeSite(ctx context.Context, s siteFiles, content []byte) error {
	if err := a.writeRootFile(s.file, content, 0o644, "root"); err != nil {
		return err
	}
	if _, err := os.Lstat(s.link); errors.Is(err, fs.ErrNotExist) {
		if err := os.Symlink(s.file, s.link); err != nil {
			a.revertSite(ctx, s)
			return err
		}
	}
	if err := a.Sys.NginxTest(ctx); err != nil {
		a.revertSite(ctx, s)
		return fmt.Errorf("nginx -t no acepta el sitio generado (se deshizo): %w", err)
	}
	return a.Sys.NginxReload(ctx)
}

// revertSite deja el sitio como estaba antes de esta corrida, y recarga nginx
// solo si lo que queda pasa nginx -t.
func (a *Accepter) revertSite(ctx context.Context, s siteFiles) {
	if !s.linked {
		_ = os.Remove(s.link)
	}
	if s.existed {
		_ = a.writeRootFile(s.file, s.before, 0o644, "root")
	} else {
		_ = os.Remove(s.file)
	}
	if err := a.Sys.NginxTest(ctx); err != nil {
		a.printf("aviso: nginx -t sigue fallando después de deshacer el sitio; no se recargó nginx: %v\n", err)
		return
	}
	_ = a.Sys.NginxReload(ctx)
}

// otherSiteFor busca un sitio habilitado, que no sea el del proyecto, cuyo
// server_name nombre el dominio.
func otherSiteFor(enabled, domain, own string) string {
	entries, err := os.ReadDir(enabled)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Name() == own {
			continue
		}
		data, err := os.ReadFile(filepath.Join(enabled, entry.Name())) //nolint:gosec // configuración de nginx, de root
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(strings.ReplaceAll(line, ";", " "))
			if len(fields) < 2 || fields[0] != "server_name" {
				continue
			}
			for _, name := range fields[1:] {
				if name == domain {
					return entry.Name()
				}
			}
		}
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Site arma, o rearma, el sitio y el certificado de un proyecto declarado:
// `sudo statussv-agent project site <nombre>`. Es el paso del alta, para
// reintentarlo cuando el DNS está listo o para un proyecto declarado a mano.
func (a *Accepter) Site(ctx context.Context, name, email string) error {
	if !a.Sys.IsRoot() {
		return ErrNotRoot
	}
	if !proto.ValidProjectName(name) {
		return errors.New("nombre de proyecto inválido")
	}
	if a.LoadSpec == nil {
		return errors.New("falta con qué leer la declaración")
	}
	spec, err := a.LoadSpec(a.Paths.Projects, name)
	if err != nil {
		return fmt.Errorf("no se pudo leer la declaración de %s: %w", name, err)
	}
	if spec.Site == nil {
		return fmt.Errorf("%s no declara un sitio (site)", name)
	}
	set, err := a.templateSet()
	if err != nil {
		return err
	}
	return a.makeSite(ctx, spec, set, email)
}
