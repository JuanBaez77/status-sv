package config_test

import (
	"log/slog"
	"os"
	"path/filepath"
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

func TestTrustedProxies(t *testing.T) {
	t.Run("por defecto, solo loopback", func(t *testing.T) {
		t.Setenv("DATABASE_URL", testDSN)

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("cargar la configuración: %v", err)
		}
		if len(cfg.TrustedProxies) != 2 {
			t.Fatalf("se esperaban las dos redes de loopback, hay %v", cfg.TrustedProxies)
		}
	})

	t.Run("lista de redes", func(t *testing.T) {
		t.Setenv("DATABASE_URL", testDSN)
		t.Setenv("STATUSSV_TRUSTED_PROXIES", " 172.30.0.0/24 , 10.0.0.0/8 ")

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("cargar la configuración: %v", err)
		}

		got := make([]string, 0, len(cfg.TrustedProxies))
		for _, prefix := range cfg.TrustedProxies {
			got = append(got, prefix.String())
		}
		if strings.Join(got, ",") != "172.30.0.0/24,10.0.0.0/8" {
			t.Errorf("redes leídas: %v", got)
		}
	})

	t.Run("normaliza la red", func(t *testing.T) {
		t.Setenv("DATABASE_URL", testDSN)
		// Una IP con máscara en vez de la dirección de red: se acepta y se normaliza.
		t.Setenv("STATUSSV_TRUSTED_PROXIES", "172.30.0.7/24")

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("cargar la configuración: %v", err)
		}
		if cfg.TrustedProxies[0].String() != "172.30.0.0/24" {
			t.Errorf("la red quedó como %v", cfg.TrustedProxies[0])
		}
	})

	for _, invalid := range []string{"172.30.0.0", "no-es-una-red", "172.30.0.0/99", " , "} {
		t.Run("rechaza "+invalid, func(t *testing.T) {
			t.Setenv("DATABASE_URL", testDSN)
			t.Setenv("STATUSSV_TRUSTED_PROXIES", invalid)

			if _, err := config.Load(); err == nil {
				t.Errorf("se aceptó %q como lista de redes", invalid)
			}
		})
	}
}

// TestMain deja una clave de TOTP válida para todos los tests: es obligatoria,
// y cada test que la quiere probar mal la pisa con t.Setenv.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "statussv-config")
	if err != nil {
		panic(err)
	}
	path := filepath.Join(dir, "totp.key")
	if err := os.WriteFile(path, []byte(testTOTPKey+"\n"), 0o600); err != nil {
		panic(err)
	}
	if err := os.Setenv("STATUSSV_TOTP_KEY_FILE", path); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// testTOTPKey son 32 bytes en base64, solo para tests.
const testTOTPKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

func TestLoadRequiresTheTOTPKey(t *testing.T) {
	t.Setenv("DATABASE_URL", testDSN)
	dir := t.TempDir()
	short := filepath.Join(dir, "corta.key")
	if err := os.WriteFile(short, []byte("MDEyMzQ1Njc4OWFiY2RlZg=="), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, path := range map[string]string{
		"sin variable":      "",
		"archivo que falta": filepath.Join(dir, "no-existe.key"),
		"clave corta":       short,
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("STATUSSV_TOTP_KEY_FILE", path)
			_, err := config.Load()
			if err == nil || !strings.Contains(err.Error(), "STATUSSV_TOTP_KEY_FILE") {
				t.Fatalf("Load = %v, se esperaba un error que nombre STATUSSV_TOTP_KEY_FILE", err)
			}
			if strings.Contains(err.Error(), testTOTPKey) {
				t.Error("el error muestra la clave")
			}
		})
	}
}
