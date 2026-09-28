// Package config resuelve la configuración del agent: de dónde sale el archivo
// de estado y cómo se registra lo que hace.
//
// Todo puede venir del entorno, porque en el VPS el agent corre bajo systemd y
// ahí es lo natural; los flags de cada subcomando pisan lo que venga del entorno.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// Rutas por defecto del archivo de estado.
const (
	// linuxStateFile es donde va en el VPS: /var/lib es el lugar estándar para
	// datos que un servicio necesita conservar entre arranques.
	linuxStateFile = "/var/lib/status-sv/agent.json"
	// devStateFile es para desarrollar fuera de Linux.
	devStateFile = "agent-state.json"
)

// Config es la configuración del agent.
type Config struct {
	// StateFile es dónde está la credencial y a qué control plane pertenece.
	StateFile string
	// ServerURL solo hace falta para enrolar; después sale del estado guardado.
	ServerURL string
	// EnrollToken solo hace falta para enrolar.
	EnrollToken string

	// DockerSocket es el socket de Docker. Si no existe, el agent informa que
	// el servidor no tiene Docker.
	DockerSocket string
	// SystemdExclude son patrones de servicios que no se muestran, además de
	// los de siempre (systemd.DefaultExclude).
	SystemdExclude []string

	LogLevel slog.Level
	LogJSON  bool
}

// Load lee la configuración del entorno.
func Load() (Config, error) {
	cfg := Config{
		StateFile:    envOr("STATUSSV_STATE_FILE", DefaultStateFile()),
		ServerURL:    strings.TrimSpace(os.Getenv("STATUSSV_SERVER_URL")),
		EnrollToken:  strings.TrimSpace(os.Getenv("STATUSSV_ENROLL_TOKEN")),
		LogJSON:      !strings.EqualFold(envOr("STATUSSV_LOG_FORMAT", "json"), "text"),
		DockerSocket: envOr("STATUSSV_DOCKER_SOCKET", "/run/docker.sock"),
	}

	level, err := envLevel("STATUSSV_LOG_LEVEL", slog.LevelInfo)
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = level

	exclude, err := envPatterns("STATUSSV_SYSTEMD_EXCLUDE")
	if err != nil {
		return Config{}, err
	}
	cfg.SystemdExclude = exclude

	return cfg, nil
}

// DefaultStateFile devuelve dónde guardar el estado según el sistema.
func DefaultStateFile() string {
	if runtime.GOOS == "linux" {
		return linuxStateFile
	}
	// Fuera de Linux el agent solo se usa para desarrollar, así que el estado
	// va junto al binario y no en una ruta del sistema.
	return filepath.Clean(devStateFile)
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
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

// envPatterns lee una lista de patrones de path.Match separados por comas, y
// rechaza los que están mal escritos: uno roto no excluiría nada sin avisar.
func envPatterns(key string) ([]string, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil, nil
	}
	var patterns []string
	for p := range strings.SplitSeq(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("%s tiene un patrón inválido (%q): %w", key, p, err)
		}
		patterns = append(patterns, p)
	}
	return patterns, nil
}
