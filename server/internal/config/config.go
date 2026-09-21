// Package config lee la configuración del control plane desde el entorno.
//
// Todo se configura por variables de entorno, nunca por archivos con secretos
// dentro del repositorio: en desarrollo salen de .env y en el VPS las pone el
// compose. La referencia de cada variable está en .env.example.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"strings"
	"time"
)

// Valores por defecto. Escuchar solo en localhost es a propósito: en producción
// el control plane va detrás de Caddy, que es quien habla con internet.
const (
	DefaultListenAddr    = "127.0.0.1:8080"
	DefaultEnrollmentTTL = 15 * time.Minute
	DefaultOfflineAfter  = 45 * time.Second
	DefaultShutdownGrace = 15 * time.Second
)

// Límites de lo que se acepta, para que un valor mal escrito falle al arrancar
// y no a las tres de la mañana.
const (
	minEnrollmentTTL = time.Minute
	maxEnrollmentTTL = time.Hour
	minOfflineAfter  = 10 * time.Second
	maxOfflineAfter  = 10 * time.Minute
)

// Config es la configuración ya validada.
type Config struct {
	// DatabaseURL es la cadena de conexión de PostgreSQL. Lleva contraseña, así
	// que nunca se escribe en un log: para eso está RedactedDatabaseURL.
	DatabaseURL string
	// ListenAddr es dónde escucha la API.
	ListenAddr string
	// EnrollmentTTL es cuánto vive un token de enrolamiento.
	EnrollmentTTL time.Duration
	// OfflineAfter es el silencio a partir del cual un servidor se considera caído.
	OfflineAfter time.Duration
	// ShutdownGrace es lo que se espera a que terminen los pedidos en curso al apagar.
	ShutdownGrace time.Duration
	// LogLevel y LogJSON controlan el registro.
	LogLevel slog.Level
	LogJSON  bool
	// TrustedProxies son las redes desde las que se acepta X-Forwarded-For. Es
	// donde está el proxy que termina TLS: en el VPS, la red interna de Docker
	// donde corre Caddy. Desde cualquier otro origen el encabezado se ignora,
	// porque lo puede escribir cualquiera.
	TrustedProxies []netip.Prefix
}

// Load arma la configuración desde el entorno y la valida.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		ListenAddr:    envOr("STATUSSV_LISTEN_ADDR", DefaultListenAddr),
		ShutdownGrace: DefaultShutdownGrace,
		LogJSON:       !strings.EqualFold(envOr("STATUSSV_LOG_FORMAT", "json"), "text"),
	}

	var err error
	if cfg.EnrollmentTTL, err = envDuration("STATUSSV_ENROLLMENT_TTL", DefaultEnrollmentTTL); err != nil {
		return Config{}, err
	}
	if cfg.OfflineAfter, err = envDuration("STATUSSV_OFFLINE_AFTER", DefaultOfflineAfter); err != nil {
		return Config{}, err
	}
	if cfg.TrustedProxies, err = envPrefixes("STATUSSV_TRUSTED_PROXIES", DefaultTrustedProxies()); err != nil {
		return Config{}, err
	}
	if cfg.LogLevel, err = envLevel("STATUSSV_LOG_LEVEL", slog.LevelInfo); err != nil {
		return Config{}, err
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate revisa que la configuración tenga sentido.
func (c Config) Validate() error {
	switch {
	case c.DatabaseURL == "":
		return errors.New("falta DATABASE_URL")
	case c.ListenAddr == "":
		return errors.New("falta STATUSSV_LISTEN_ADDR")
	case c.EnrollmentTTL < minEnrollmentTTL || c.EnrollmentTTL > maxEnrollmentTTL:
		return fmt.Errorf("STATUSSV_ENROLLMENT_TTL debe estar entre %v y %v, llegó %v",
			minEnrollmentTTL, maxEnrollmentTTL, c.EnrollmentTTL)
	case c.OfflineAfter < minOfflineAfter || c.OfflineAfter > maxOfflineAfter:
		return fmt.Errorf("STATUSSV_OFFLINE_AFTER debe estar entre %v y %v, llegó %v",
			minOfflineAfter, maxOfflineAfter, c.OfflineAfter)
	}
	return nil
}

// RedactedDatabaseURL devuelve la cadena de conexión sin la contraseña, para
// poder registrarla al arrancar sin filtrar nada.
func (c Config) RedactedDatabaseURL() string {
	return redactDSN(c.DatabaseURL)
}

// redactDSN reemplaza la contraseña de una URL de conexión por una marca.
// Trabaja sobre el texto y no con net/url a propósito: si el DSN está mal
// formado, igual tiene que ocultar lo que haya entre ':' y '@'.
func redactDSN(dsn string) string {
	schemeEnd := strings.Index(dsn, "://")
	if schemeEnd < 0 {
		return "[cadena de conexión oculta]"
	}
	rest := dsn[schemeEnd+3:]

	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return dsn // no hay credenciales que ocultar
	}
	userInfo := rest[:at]

	colon := strings.Index(userInfo, ":")
	if colon < 0 {
		return dsn // hay usuario pero no contraseña
	}
	return dsn[:schemeEnd+3] + userInfo[:colon] + ":[contraseña oculta]@" + rest[at+1:]
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s no es una duración válida (%q): %w", key, raw, err)
	}
	return value, nil
}

func envLevel(key string, fallback slog.Level) (slog.Level, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		return 0, fmt.Errorf("%s no es un nivel de log válido (%q): usar debug, info, warn o error", key, raw)
	}
	return level, nil
}

// DefaultTrustedProxies es solo loopback: sirve cuando el proxy corre en el
// mismo host que el control plane, fuera de Docker.
func DefaultTrustedProxies() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("::1/128"),
	}
}

// envPrefixes lee una lista de redes en notación CIDR separadas por comas.
func envPrefixes(key string, fallback []netip.Prefix) ([]netip.Prefix, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}

	var prefixes []netip.Prefix
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("%s tiene una red inválida (%q): se espera notación CIDR, como 172.30.0.0/24", key, part)
		}
		prefixes = append(prefixes, prefix.Masked())
	}

	if len(prefixes) == 0 {
		return nil, fmt.Errorf("%s no tiene ninguna red", key)
	}
	return prefixes, nil
}
