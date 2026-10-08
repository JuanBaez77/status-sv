package proto

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Límites del alta de proyectos (ADR-022 y ADR-023).
const (
	// MaxProposalBytes es lo más que pesa la declaración de una propuesta. Es
	// la mitad de lo que acepta un project.yaml escrito a mano: así, aun con
	// cada carácter escapado por JSON (hasta 6 bytes), entra en un mensaje. Una
	// real pesa menos de 2 KiB.
	MaxProposalBytes = 32 << 10
	// MaxPendingProposals es cuántas propuestas guarda un agent a la vez.
	MaxPendingProposals = 10
	// MaxRequiredEnv es cuántas variables requeridas puede declarar un
	// proyecto. Sus nombres pueden viajar en projects_state, uno por cada
	// proyecto del servidor: el tope cuida el tamaño de ese mensaje.
	MaxRequiredEnv = 20
	// MaxEnvNameLen acota el nombre de una variable requerida.
	MaxEnvNameLen = 64
	// MinSitePort y MaxSitePort acotan el puerto de 127.0.0.1 al que pasa los
	// pedidos el sitio generado: nunca uno reservado.
	MinSitePort = 1024
	MaxSitePort = 65535

	maxDomainLen    = 253
	maxPublicKeyLen = 1024
)

// ProposalAction es lo que el control plane le pide al agent con una propuesta.
type ProposalAction string

// Acciones sobre una propuesta.
const (
	// ProposalPut guarda la propuesta, o la reemplaza si ya había una con el
	// mismo nombre.
	ProposalPut ProposalAction = "put"
	// ProposalWithdraw la borra: se retiró o venció.
	ProposalWithdraw ProposalAction = "withdraw"
)

// ProjectProposal es la propuesta de un proyecto nuevo (ADR-022). No es un
// comando: el agent no ejecuta nada con ella. La guarda en su directorio de
// estado hasta que una persona la acepta en el servidor con
// `statussv-agent project accept`, que es la que declara el proyecto.
//
// YAML es la declaración completa, en el formato de un project.yaml. El agent
// la valida con el mismo parser estricto antes de guardarla, y el comando de
// aceptación la valida otra vez: para los dos es texto no confiable.
type ProjectProposal struct {
	ProposalID uuid.UUID      `json:"proposal_id"`
	Name       string         `json:"name"`
	Action     ProposalAction `json:"action"`
	YAML       string         `json:"yaml,omitempty"`
	ExpiresAt  time.Time      `json:"expires_at,omitzero"`
}

// Type implementa Message.
func (ProjectProposal) Type() MessageType { return TypeProjectProposal }

// Validate implementa Message. Revisa la forma; que la declaración sea un
// project.yaml válido lo revisa el agent con su parser.
func (p ProjectProposal) Validate() error {
	switch {
	case p.ProposalID == uuid.Nil:
		return errors.New("falta el ID de la propuesta")
	case !ValidProjectName(p.Name):
		return errors.New("nombre de proyecto inválido")
	}
	switch p.Action {
	case ProposalPut:
		switch {
		case p.YAML == "":
			return errors.New("la propuesta no trae la declaración")
		case len(p.YAML) > MaxProposalBytes:
			return fmt.Errorf("la declaración pesa %d bytes y el máximo es %d", len(p.YAML), MaxProposalBytes)
		case !utf8.ValidString(p.YAML):
			return errors.New("la declaración no es UTF-8 válido")
		case p.ExpiresAt.IsZero():
			return errors.New("falta el vencimiento")
		}
	case ProposalWithdraw:
		if p.YAML != "" || !p.ExpiresAt.IsZero() {
			return errors.New("retirar una propuesta no lleva declaración ni vencimiento")
		}
	default:
		return fmt.Errorf("acción desconocida %q", p.Action)
	}
	return nil
}

// ProposalResult es cómo terminó una propuesta en el agent.
type ProposalResult string

// Resultados de una propuesta.
const (
	ProposalStored    ProposalResult = "stored"
	ProposalWithdrawn ProposalResult = "withdrawn"
	// ProposalRejected es una propuesta que el agent no guardó: no pasa el
	// parser, no cumple lo que una propuesta no elige, ya hay un proyecto
	// declarado con ese nombre, venció o ya tiene el máximo guardado. Error
	// dice cuál, sin los valores de la declaración.
	ProposalRejected ProposalResult = "rejected"
)

// ProjectProposalAck es la respuesta del agent a una propuesta.
type ProjectProposalAck struct {
	ProposalID uuid.UUID      `json:"proposal_id"`
	Name       string         `json:"name"`
	Result     ProposalResult `json:"result"`
	Error      string         `json:"error,omitempty"`
}

// Type implementa Message.
func (ProjectProposalAck) Type() MessageType { return TypeProjectProposalAck }

// Validate implementa Message.
func (a ProjectProposalAck) Validate() error {
	switch {
	case a.ProposalID == uuid.Nil:
		return errors.New("falta el ID de la propuesta")
	case !ValidProjectName(a.Name):
		return errors.New("nombre de proyecto inválido")
	case a.Result != ProposalStored && a.Result != ProposalWithdrawn && a.Result != ProposalRejected:
		return fmt.Errorf("resultado desconocido %q", a.Result)
	case a.Result == ProposalRejected && a.Error == "":
		return errors.New("un rechazo tiene que decir por qué")
	case a.Result != ProposalRejected && a.Error != "":
		return errors.New("solo un rechazo trae un error")
	case len(a.Error) > MaxProjectErrorLen || !isPrintable(a.Error):
		return errors.New("explicación inválida")
	}
	return nil
}

// PendingProposal es una propuesta que el agent tiene guardada. Viaja en
// projects_state para que el control plane sepa qué llegó y reenvíe lo que
// falte.
type PendingProposal struct {
	ProposalID uuid.UUID `json:"proposal_id"`
	Name       string    `json:"name"`
	Hash       string    `json:"hash"`
}

// Validate revisa el ID, el nombre y el hash.
func (p PendingProposal) Validate() error {
	switch {
	case p.ProposalID == uuid.Nil:
		return errors.New("falta el ID de la propuesta")
	case !ValidProjectName(p.Name):
		return errors.New("nombre de proyecto inválido")
	case !validHash(p.Hash):
		return errors.New("hash inválido")
	}
	return nil
}

// SpecHash es el hash de una declaración: SHA-256 en hexadecimal. El de una
// propuesta y el del project.yaml que se escribió al aceptarla son iguales si
// nadie la cambió.
func SpecHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validHash(hash string) bool {
	return hashPattern.MatchString(hash)
}

// SiteSpec es el sitio de nginx de un proyecto (ADR-023): el dominio y el
// puerto de 127.0.0.1 al que pasa los pedidos.
type SiteSpec struct {
	Domain string `json:"domain"`
	Port   int    `json:"port"`
}

// Validate revisa el dominio y el puerto.
func (s SiteSpec) Validate() error {
	switch {
	case !ValidDomain(s.Domain):
		return fmt.Errorf("domain: tiene que ser un nombre de host en minúsculas, con al menos un punto, sin comodines y de hasta %d caracteres", maxDomainLen)
	case s.Port < MinSitePort || s.Port > MaxSitePort:
		return fmt.Errorf("port: tiene que estar entre %d y %d", MinSitePort, MaxSitePort)
	}
	return nil
}

// domainLabel es una etiqueta de un nombre de host en minúsculas: letras,
// números y guiones, sin guion en las puntas.
var domainLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidDomain dice si un dominio se puede usar en el sitio generado: un nombre
// de host en minúsculas, con al menos dos etiquetas, sin comodines y que no es
// una dirección IP. Es lo único del dominio que llega a la plantilla de nginx.
func ValidDomain(domain string) bool {
	if domain == "" || len(domain) > maxDomainLen {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if !domainLabel.MatchString(label) {
			return false
		}
	}
	// La última etiqueta no es solo números: eso es una dirección IP.
	return strings.Trim(labels[len(labels)-1], "0123456789") != ""
}

// envNamePattern es el nombre de una variable que se puede declarar como
// requerida: mayúsculas, números y _, sin empezar con número.
var envNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// ValidEnvName dice si un nombre de variable se puede declarar en required_env.
func ValidEnvName(name string) bool {
	return len(name) <= MaxEnvNameLen && envNamePattern.MatchString(name)
}

// EnvStatus dice si están cargadas las variables requeridas de un proyecto
// (ADR-022). Viajan nombres, nunca valores.
type EnvStatus struct {
	// Required es cuántas declara el proyecto.
	Required int `json:"required"`
	// Missing son las que faltan.
	Missing []string `json:"missing,omitempty"`
	// Unknown dice que el agent no pudo mirar: con systemd, el archivo de
	// entorno es de root y lo mira el ayudante, que puede no estar instalado.
	Unknown bool `json:"unknown,omitempty"`
}

// Validate revisa los rangos y los nombres.
func (e EnvStatus) Validate() error {
	switch {
	case e.Required < 1 || e.Required > MaxRequiredEnv:
		return fmt.Errorf("required: tiene que estar entre 1 y %d", MaxRequiredEnv)
	case len(e.Missing) > e.Required:
		return errors.New("missing: faltan más de las que se declaran")
	case e.Unknown && len(e.Missing) > 0:
		return errors.New("missing: no puede saberse qué falta si no se pudo mirar")
	}
	seen := make(map[string]bool, len(e.Missing))
	for _, name := range e.Missing {
		if !ValidEnvName(name) {
			return errors.New("missing: nombre de variable inválido")
		}
		if seen[name] {
			return errors.New("missing: hay un nombre repetido")
		}
		seen[name] = true
	}
	return nil
}

// publicKeyTypes son los tipos de llave SSH que puede tener una deploy key.
var publicKeyTypes = []string{
	"ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521",
}

var base64Pattern = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)

// ValidPublicKey dice si una llave pública SSH tiene la forma "tipo base64",
// sin comentario: es lo que el panel muestra para cargar en GitHub.
func ValidPublicKey(key string) bool {
	if len(key) > maxPublicKeyLen {
		return false
	}
	kind, data, ok := strings.Cut(key, " ")
	if !ok || !base64Pattern.MatchString(data) {
		return false
	}
	for _, known := range publicKeyTypes {
		if kind == known {
			return true
		}
	}
	return false
}
