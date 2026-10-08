// Package templates genera los dos archivos que el alta de un proyecto escribe
// en el servidor (ADR-023): la unidad de systemd de su servicio y su sitio de
// nginx.
//
// El texto de las plantillas vive en el repo, embebido en el binario, y se
// cambia por PR. Un servidor puede reemplazarlas con archivos propios en
// /etc/status-sv/templates, que solo se usan si son de root y nadie más los
// puede escribir. Lo que llega a una plantilla son parámetros que ya pasaron
// el parser estricto del project.yaml; este paquete los revisa otra vez y los
// escapa para el formato de cada archivo, porque un error acá termina en un
// archivo que lee root.
package templates

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"unicode"

	"github.com/JuanBaez77/status-sv/internal/projectspec"
	"github.com/JuanBaez77/status-sv/proto"
)

// Nombres de las plantillas, en el binario y en el directorio de reemplazo.
const (
	ServiceTemplate = "service.tmpl"
	SiteTemplate    = "nginx-site.tmpl"
)

// Rutas del servidor que usan los archivos generados.
const (
	// OverrideDir es donde un servidor puede dejar sus propias plantillas.
	OverrideDir = "/etc/status-sv/templates"
	// ACMERoot es de donde nginx sirve el desafío de Let's Encrypt.
	ACMERoot = "/var/www/statussv-acme"
	// LetsEncryptLive es donde certbot deja los certificados.
	LetsEncryptLive = "/etc/letsencrypt/live"
)

// Factory es el origen de una plantilla que no se reemplazó.
const Factory = "de fábrica"

//go:embed service.tmpl nginx-site.tmpl
var factory embed.FS

// Set son las dos plantillas con las que se generan los archivos de un
// proyecto, y las rutas que usan.
type Set struct {
	service *template.Template
	site    *template.Template
	// ServiceOrigin y SiteOrigin dicen de dónde salió cada plantilla: Factory
	// o la ruta del archivo del servidor. El comando de aceptación lo muestra.
	ServiceOrigin string
	SiteOrigin    string
	// ACMERoot y LiveDir son las rutas del desafío y de los certificados.
	// Cambian solo en las pruebas.
	ACMERoot string
	LiveDir  string
}

// Default devuelve las plantillas de fábrica.
func Default() *Set {
	set, err := load(factory, ServiceTemplate, SiteTemplate)
	if err != nil {
		// Las plantillas de fábrica se prueban en cada PR: no compilar es un
		// error de programación.
		panic(err)
	}
	set.ServiceOrigin, set.SiteOrigin = Factory, Factory
	return set
}

// Load devuelve las plantillas, con las del directorio dir en lugar de las de
// fábrica cuando existen. trusted revisa cada archivo de dir antes de usarlo
// (que sea de root y nadie más lo pueda escribir): si no lo es, Load falla en
// vez de volver a la de fábrica, porque alguien quiso cambiarla.
func Load(dir string, trusted func(path string) error) (*Set, error) {
	set := Default()
	for _, name := range []string{ServiceTemplate, SiteTemplate} {
		file := filepath.Join(dir, name)
		if _, err := os.Lstat(file); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("mirar %s: %w", file, err)
		}
		if err := trusted(dir); err != nil {
			return nil, err
		}
		if err := trusted(file); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(file) //nolint:gosec // ruta fija, de root
		if err != nil {
			return nil, fmt.Errorf("leer %s: %w", file, err)
		}
		tmpl, err := parse(name, string(data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		if name == ServiceTemplate {
			set.service, set.ServiceOrigin = tmpl, file
		} else {
			set.site, set.SiteOrigin = tmpl, file
		}
	}
	return set, nil
}

func load(fsys fs.FS, serviceName, siteName string) (*Set, error) {
	set := &Set{ACMERoot: ACMERoot, LiveDir: LetsEncryptLive}
	for _, name := range []string{serviceName, siteName} {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		tmpl, err := parse(name, string(data))
		if err != nil {
			return nil, err
		}
		if name == serviceName {
			set.service = tmpl
		} else {
			set.site = tmpl
		}
	}
	return set, nil
}

// parse compila una plantilla. Una clave que falta es un error: una plantilla
// del servidor escrita para otra versión no genera un archivo a medias.
func parse(name, text string) (*template.Template, error) {
	return template.New(name).Option("missingkey=error").Parse(text)
}

// ServiceUnitFile es el nombre del archivo de la unidad de un proyecto.
func ServiceUnitFile(project string) string {
	return projectspec.ServiceUnit(project)
}

// SiteFileName es el nombre del archivo del sitio de un proyecto en
// sites-available. El prefijo dice quién lo generó.
func SiteFileName(project string) string {
	return "statussv-" + project + ".conf"
}

// CertName es el nombre del certificado de un proyecto para certbot: su
// carpeta en /etc/letsencrypt/live. Fijarlo evita los "-0001" de certbot.
func CertName(project string) string {
	return "statussv-" + project
}

// serviceData es lo que recibe la plantilla de la unidad: todo ya revisado y,
// ExecStart, escapado.
type serviceData struct {
	Project   string
	User      string
	Workdir   string
	EnvFile   string
	ExecStart string
}

// unitPath es una ruta que se puede escribir tal cual en una unidad: absoluta,
// sin espacios, comillas, % ni $, que systemd interpretaría.
var unitPath = regexp.MustCompile(`^/[A-Za-z0-9._/-]*$`)

// unitUser es un usuario del sistema, como lo valida el parser.
var unitUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// Service genera la unidad del servicio de un proyecto systemd que declara
// service (ADR-023).
func (s *Set) Service(spec projectspec.Spec) ([]byte, error) {
	project := spec.Project
	switch {
	case project.Runtime != proto.RuntimeSystemd || spec.Service == nil:
		return nil, errors.New("el proyecto no declara un servicio de systemd que generar")
	case !proto.ValidProjectName(project.Name):
		return nil, errors.New("nombre de proyecto inválido")
	case !unitUser.MatchString(spec.RunAs) || spec.RunAs == "root" || spec.RunAs == projectspec.AgentUser:
		return nil, errors.New("run_as no se puede escribir en una unidad")
	case !cleanUnitPath(project.Workdir):
		return nil, errors.New("el workdir no se puede escribir en una unidad: solo letras, números, . _ - y /")
	case spec.EnvFile != "" && !cleanUnitPath(spec.EnvFile):
		return nil, errors.New("el env_file no se puede escribir en una unidad: solo letras, números, . _ - y /")
	case !path.IsAbs(spec.Service.Exec):
		return nil, errors.New("el programa del servicio no es una ruta absoluta")
	}
	args := append([]string{spec.Service.Exec}, spec.Service.Command[1:]...)
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		q, err := systemdQuote(arg)
		if err != nil {
			return nil, err
		}
		quoted = append(quoted, q)
	}
	return execute(s.service, serviceData{
		Project:   project.Name,
		User:      spec.RunAs,
		Workdir:   project.Workdir,
		EnvFile:   spec.EnvFile,
		ExecStart: strings.Join(quoted, " "),
	})
}

func cleanUnitPath(p string) bool {
	return unitPath.MatchString(p) && path.Clean(p) == p && p != "/"
}

// systemdArg escapa lo que systemd interpreta adentro de comillas dobles: la
// barra invertida y la comilla, los especificadores (%) y las variables ($).
var systemdArg = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)

// systemdQuote escribe un argumento para ExecStart: entre comillas dobles, con
// lo que systemd interpreta escapado. Un carácter de control no se puede
// escribir y es un error, no algo que se pasa.
func systemdQuote(arg string) (string, error) {
	if arg == "" {
		return "", errors.New("argumento vacío en el comando del servicio")
	}
	for _, r := range arg {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return "", errors.New("el comando del servicio tiene caracteres de control")
		}
	}
	return `"` + systemdArg.Replace(arg) + `"`, nil
}

// SitePhase es cuál de los dos sitios se genera (ADR-023, punto 5).
type SitePhase int

// Fases del sitio.
const (
	// SiteHTTP sirve solo el desafío de Let's Encrypt, antes del certificado.
	SiteHTTP SitePhase = iota
	// SiteTLS es el sitio completo: el 80 redirige y el 443 pasa al proyecto.
	SiteTLS
)

// siteData es lo que recibe la plantilla del sitio.
type siteData struct {
	Project  string
	Domain   string
	Port     int
	TLS      bool
	ACMERoot string
	CertDir  string
	// UpgradeVar es una variable de nginx propia del proyecto, para el map
	// del WebSocket: dos sitios no pueden definir la misma.
	UpgradeVar string
}

// Site genera el sitio de nginx de un proyecto que declara site (ADR-023).
func (s *Set) Site(spec projectspec.Spec, phase SitePhase) ([]byte, error) {
	project := spec.Project
	switch {
	case spec.Site == nil:
		return nil, errors.New("el proyecto no declara un sitio")
	case !proto.ValidProjectName(project.Name):
		return nil, errors.New("nombre de proyecto inválido")
	case !cleanUnitPath(s.ACMERoot) || !cleanUnitPath(s.LiveDir):
		return nil, errors.New("rutas del desafío o de los certificados inválidas")
	}
	if err := (proto.SiteSpec{Domain: spec.Site.Domain, Port: spec.Site.Port}).Validate(); err != nil {
		return nil, fmt.Errorf("site.%w", err)
	}
	return execute(s.site, siteData{
		Project:    project.Name,
		Domain:     spec.Site.Domain,
		Port:       spec.Site.Port,
		TLS:        phase == SiteTLS,
		ACMERoot:   s.ACMERoot,
		CertDir:    s.LiveDir + "/" + CertName(project.Name),
		UpgradeVar: "statussv_upgrade_" + strings.ReplaceAll(project.Name, "-", "_"),
	})
}

func execute(tmpl *template.Template, data any) ([]byte, error) {
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return nil, fmt.Errorf("plantilla %s: %w", tmpl.Name(), err)
	}
	return out.Bytes(), nil
}
