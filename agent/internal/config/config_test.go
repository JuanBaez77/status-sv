package config_test

import (
	"log/slog"
	"runtime"
	"strings"
	"testing"

	"github.com/JuanBaez77/status-sv/agent/internal/config"
)

func TestLoadUsesDefaults(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("cargar la configuración: %v", err)
	}

	if cfg.StateFile != config.DefaultStateFile() {
		t.Errorf("StateFile = %q, se esperaba %q", cfg.StateFile, config.DefaultStateFile())
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v", cfg.LogLevel)
	}
	if !cfg.LogJSON {
		t.Error("el formato por defecto debería ser JSON, que es lo que lee journald")
	}
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv("STATUSSV_STATE_FILE", "/tmp/otro-estado.json")
	t.Setenv("STATUSSV_SERVER_URL", "https://status.example.com")
	t.Setenv("STATUSSV_ENROLL_TOKEN", "svenr_desde-el-entorno")
	t.Setenv("STATUSSV_LOG_LEVEL", "debug")
	t.Setenv("STATUSSV_LOG_FORMAT", "text")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("cargar la configuración: %v", err)
	}

	if cfg.StateFile != "/tmp/otro-estado.json" {
		t.Errorf("StateFile = %q", cfg.StateFile)
	}
	if cfg.ServerURL != "https://status.example.com" {
		t.Errorf("ServerURL = %q", cfg.ServerURL)
	}
	if cfg.EnrollToken != "svenr_desde-el-entorno" {
		t.Errorf("EnrollToken = %q", cfg.EnrollToken)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v", cfg.LogLevel)
	}
	if cfg.LogJSON {
		t.Error("STATUSSV_LOG_FORMAT=text debería apagar el JSON")
	}
}

func TestLoadRejectsBadLogLevel(t *testing.T) {
	t.Setenv("STATUSSV_LOG_LEVEL", "susurrando")

	_, err := config.Load()
	if err == nil {
		t.Fatal("se aceptó un nivel de log inventado")
	}
	if !strings.Contains(err.Error(), "STATUSSV_LOG_LEVEL") {
		t.Errorf("el error no menciona la variable: %v", err)
	}
}

// En el VPS el estado va a una ruta del sistema; fuera de Linux, junto al binario.
func TestDefaultStateFileDependsOnTheSystem(t *testing.T) {
	got := config.DefaultStateFile()

	if runtime.GOOS == "linux" {
		if got != "/var/lib/status-sv/agent.json" {
			t.Errorf("en Linux el estado va a /var/lib/status-sv/agent.json, no a %q", got)
		}
		return
	}
	if strings.HasPrefix(got, "/var/lib") {
		t.Errorf("fuera de Linux no corresponde una ruta del sistema: %q", got)
	}
}

// Los valores del entorno con espacios de más no pueden romper la configuración.
func TestLoadTrimsWhitespace(t *testing.T) {
	t.Setenv("STATUSSV_SERVER_URL", "  https://status.example.com  ")
	t.Setenv("STATUSSV_ENROLL_TOKEN", "  svenr_con-espacios  ")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("cargar la configuración: %v", err)
	}
	if cfg.ServerURL != "https://status.example.com" {
		t.Errorf("ServerURL = %q", cfg.ServerURL)
	}
	if cfg.EnrollToken != "svenr_con-espacios" {
		t.Errorf("EnrollToken = %q", cfg.EnrollToken)
	}
}
