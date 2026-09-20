package config_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/server/internal/config"
)

const testDSN = "postgres://statussv:s3cr3t0@127.0.0.1:5433/statussv?sslmode=disable"

func TestLoadUsesDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", testDSN)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("cargar la configuración: %v", err)
	}

	if cfg.ListenAddr != config.DefaultListenAddr {
		t.Errorf("ListenAddr = %q, se esperaba %q", cfg.ListenAddr, config.DefaultListenAddr)
	}
	if cfg.EnrollmentTTL != config.DefaultEnrollmentTTL {
		t.Errorf("EnrollmentTTL = %v, se esperaba %v", cfg.EnrollmentTTL, config.DefaultEnrollmentTTL)
	}
	if cfg.OfflineAfter != config.DefaultOfflineAfter {
		t.Errorf("OfflineAfter = %v, se esperaba %v", cfg.OfflineAfter, config.DefaultOfflineAfter)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, se esperaba info", cfg.LogLevel)
	}
	if !cfg.LogJSON {
		t.Error("el formato de log por defecto debería ser JSON")
	}
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv("DATABASE_URL", testDSN)
	t.Setenv("STATUSSV_LISTEN_ADDR", "0.0.0.0:9000")
	t.Setenv("STATUSSV_ENROLLMENT_TTL", "5m")
	t.Setenv("STATUSSV_OFFLINE_AFTER", "30s")
	t.Setenv("STATUSSV_LOG_LEVEL", "debug")
	t.Setenv("STATUSSV_LOG_FORMAT", "text")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("cargar la configuración: %v", err)
	}

	if cfg.ListenAddr != "0.0.0.0:9000" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.EnrollmentTTL != 5*time.Minute {
		t.Errorf("EnrollmentTTL = %v", cfg.EnrollmentTTL)
	}
	if cfg.OfflineAfter != 30*time.Second {
		t.Errorf("OfflineAfter = %v", cfg.OfflineAfter)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v", cfg.LogLevel)
	}
	if cfg.LogJSON {
		t.Error("STATUSSV_LOG_FORMAT=text debería apagar el JSON")
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "sin base de datos",
			env:  map[string]string{"DATABASE_URL": ""},
			want: "DATABASE_URL",
		},
		{
			name: "token que vive demasiado",
			env:  map[string]string{"DATABASE_URL": testDSN, "STATUSSV_ENROLLMENT_TTL": "24h"},
			want: "STATUSSV_ENROLLMENT_TTL",
		},
		{
			name: "token que vive muy poco",
			env:  map[string]string{"DATABASE_URL": testDSN, "STATUSSV_ENROLLMENT_TTL": "5s"},
			want: "STATUSSV_ENROLLMENT_TTL",
		},
		{
			name: "duración mal escrita",
			env:  map[string]string{"DATABASE_URL": testDSN, "STATUSSV_OFFLINE_AFTER": "45 segundos"},
			want: "STATUSSV_OFFLINE_AFTER",
		},
		{
			name: "nivel de log inventado",
			env:  map[string]string{"DATABASE_URL": testDSN, "STATUSSV_LOG_LEVEL": "gritando"},
			want: "STATUSSV_LOG_LEVEL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for key, value := range tt.env {
				t.Setenv(key, value)
			}

			_, err := config.Load()
			if err == nil {
				t.Fatal("se esperaba un error de configuración")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("el error no menciona %q: %v", tt.want, err)
			}
		})
	}
}

// La cadena de conexión se registra al arrancar, así que la contraseña no puede
// aparecer nunca en los logs.
func TestRedactedDatabaseURL(t *testing.T) {
	tests := []struct {
		name     string
		dsn      string
		contains string
		hides    string
	}{
		{
			name:     "dsn normal",
			dsn:      testDSN,
			contains: "postgres://statussv:[contraseña oculta]@127.0.0.1:5433/statussv?sslmode=disable",
			hides:    "s3cr3t0",
		},
		{
			name:     "sin contraseña",
			dsn:      "postgres://statussv@127.0.0.1:5433/statussv",
			contains: "postgres://statussv@127.0.0.1:5433/statussv",
		},
		{
			name:     "sin credenciales",
			dsn:      "postgres://127.0.0.1:5433/statussv",
			contains: "postgres://127.0.0.1:5433/statussv",
		},
		{
			name:     "contraseña con arroba adentro",
			dsn:      "postgres://statussv:pa@ss@127.0.0.1:5433/statussv",
			contains: "[contraseña oculta]",
			hides:    "pa@ss",
		},
		{
			name:     "formato clave=valor",
			dsn:      "host=127.0.0.1 user=statussv password=s3cr3t0",
			contains: "[cadena de conexión oculta]",
			hides:    "s3cr3t0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{DatabaseURL: tt.dsn}
			got := cfg.RedactedDatabaseURL()

			if !strings.Contains(got, tt.contains) {
				t.Errorf("RedactedDatabaseURL() = %q, se esperaba que contuviera %q", got, tt.contains)
			}
			if tt.hides != "" && strings.Contains(got, tt.hides) {
				t.Errorf("RedactedDatabaseURL() filtró %q: %q", tt.hides, got)
			}
		})
	}
}
