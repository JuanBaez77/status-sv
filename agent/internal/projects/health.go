package projects

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/JuanBaez77/status-sv/proto"
)

// DefaultHealthInterval es cada cuánto se prueba el healthcheck declarado de
// cada proyecto (ADR-012).
const DefaultHealthInterval = 60 * time.Second

// maxLatency es el techo de la latencia que acepta el contrato.
const maxLatency = 60 * time.Second

// HealthChecker prueba el healthcheck de un proyecto. Nunca falla: lo que salga
// mal queda en el resultado como failing.
type HealthChecker interface {
	Check(ctx context.Context, spec proto.HealthcheckSpec) proto.HealthResult
}

// Checker es el healthcheck pasivo por HTTP: un GET a la URL declarada, con el
// timeout del archivo, verificando el certificado y sin seguir redirecciones.
// De la respuesta solo mira el código y el tiempo; el cuerpo no se lee.
type Checker struct {
	client    *http.Client
	userAgent string
	now       func() time.Time
}

// NewChecker arma el Checker. version va en el User-Agent, para que quien mire
// los registros del proyecto sepa de dónde salen esos pedidos.
func NewChecker(version string) *Checker {
	transport := &http.Transport{
		// El healthcheck sale directo desde el servidor, aunque el entorno
		// tenga un proxy: se prueba lo que el servidor ve.
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: maxLatency}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: maxLatency,
		ForceAttemptHTTP2:   true,
		// Una prueba por minuto no justifica guardar conexiones abiertas, y
		// así cada prueba incluye la conexión y el certificado.
		DisableKeepAlives:  true,
		DisableCompression: true,
	}
	return &Checker{
		client: &http.Client{
			Transport: transport,
			// Una redirección vale como su código: seguirla podría llevar al
			// agent a cualquier host (ADR-012).
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		userAgent: "status-sv-agent/" + version,
		now:       time.Now,
	}
}

// Check hace el GET y arma el resultado.
func (c *Checker) Check(ctx context.Context, spec proto.HealthcheckSpec) proto.HealthResult {
	timeout := time.Duration(spec.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := c.now()
	result := proto.HealthResult{State: proto.HealthFailing, CheckedAt: start.UTC()}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.URL, http.NoBody)
	if err != nil {
		result.Detail = "no se pudo armar el pedido"
		return result
	}
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.client.Do(req)
	result.LatencyMS = int(min(max(c.now().Sub(start), 0), maxLatency).Milliseconds())
	if err != nil {
		result.Detail = failureDetail(err, spec.TimeoutSeconds)
		return result
	}
	// El cuerpo se descarta sin leerlo.
	_ = resp.Body.Close()

	result.StatusCode = resp.StatusCode
	if resp.StatusCode == spec.ExpectStatus {
		result.State = proto.HealthPassing
		return result
	}
	result.Detail = fmt.Sprintf("respondió %d y se esperaba %d", resp.StatusCode, spec.ExpectStatus)
	return result
}

// failureDetail explica por qué no hubo respuesta con textos fijos: el error de
// Go repite la URL y puede crecer más de lo que acepta el contrato.
func failureDetail(err error, timeoutSeconds int) string {
	var (
		dnsErr     *net.DNSError
		unknownCA  x509.UnknownAuthorityError
		hostErr    x509.HostnameError
		invalidErr x509.CertificateInvalidError
		verifyErr  *tls.CertificateVerificationError
	)
	switch {
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return fmt.Sprintf("no respondió en %d s", timeoutSeconds)
	case errors.Is(err, context.Canceled):
		return "se canceló la prueba"
	case errors.As(err, &verifyErr) || errors.As(err, &unknownCA) ||
		errors.As(err, &hostErr) || errors.As(err, &invalidErr):
		return "certificado inválido"
	case errors.As(err, &dnsErr):
		return "no se pudo resolver el nombre"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "conexión rechazada"
	case errors.Is(err, syscall.ECONNRESET):
		return "el servidor cortó la conexión"
	case errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH):
		return "no hay ruta hasta el servidor"
	default:
		return "no se pudo completar el pedido"
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
