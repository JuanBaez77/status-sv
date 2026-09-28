package redact

import (
	"strings"
	"testing"
)

// Cada caso trae el secreto que no puede sobrevivir. Se verifica eso además del
// resultado exacto, porque es lo que de verdad importa.
func TestString(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		secret string
	}{
		{
			name:   "opción con igual",
			in:     "/usr/bin/app --db-password=Sup3rS3creta --port 8000",
			want:   "/usr/bin/app --db-password=*** --port 8000",
			secret: "Sup3rS3creta",
		},
		{
			name:   "opción con el valor aparte",
			in:     "worker --api-key abcdef123456 --verbose",
			want:   "worker --api-key *** --verbose",
			secret: "abcdef123456",
		},
		{
			name:   "opción con valor entre comillas",
			in:     `app --secret "dos palabras" -v`,
			want:   `app --secret *** -v`,
			secret: "dos palabras",
		},
		{
			name:   "variable de entorno",
			in:     "DB_PASSWORD=hunter2 DEBUG=1 gunicorn app:wsgi",
			want:   "DB_PASSWORD=*** DEBUG=1 gunicorn app:wsgi",
			secret: "hunter2",
		},
		{
			name:   "JSON",
			in:     `{"user": "juan", "api_key": "k-123456789", "ok": true}`,
			want:   `{"user": "juan", "api_key": ***, "ok": true}`,
			secret: "k-123456789",
		},
		{
			name:   "YAML",
			in:     "client_secret: 9f8e7d6c5b4a",
			want:   "client_secret: ***",
			secret: "9f8e7d6c5b4a",
		},
		{
			name:   "credenciales en una URL",
			in:     "conectando a postgres://app:Cl4ve@db:5432/prod",
			want:   "conectando a postgres://app:***@db:5432/prod",
			secret: "Cl4ve",
		},
		{
			name:   "URL de Redis sin usuario",
			in:     "redis://:otraClave@cache:6379/0",
			want:   "redis://:***@cache:6379/0",
			secret: "otraClave",
		},
		{
			name:   "token en la query de una URL",
			in:     "GET /hook?token=abc123def456&page=2 200",
			want:   "GET /hook?token=***&page=2 200",
			secret: "abc123def456",
		},
		{
			name: "encabezado Bearer",
			in:   "Authorization: Bearer eyAbCdEfGh0123456789",
			// La palabra "Authorization" también delata un secreto, así que se tapa
			// además el tipo. Queda menos legible, pero no se escapa nada.
			want:   "Authorization: *** ***",
			secret: "eyAbCdEfGh0123456789",
		},
		{
			name:   "mysql con la clave pegada",
			in:     "mysqldump -u root -pRootPass123 cristales",
			want:   "mysqldump -u root -p*** cristales",
			secret: "RootPass123",
		},
		{
			name:   "sshpass",
			in:     "sshpass -p laclave ssh deploy@host",
			want:   "sshpass -p *** ssh deploy@host",
			secret: "laclave",
		},
		{
			name:   "token de GitHub suelto",
			in:     "git clone https://x-access:ghp_ABCDEFGHIJKLMNOPQRSTUVWX0123@github.com/o/r",
			secret: "ghp_ABCDEFGHIJKLMNOPQRSTUVWX0123",
		},
		{
			name:   "token de GitHub sin contexto",
			in:     "usando ghp_ABCDEFGHIJKLMNOPQRSTUVWX0123 para el clon",
			want:   "usando *** para el clon",
			secret: "ghp_ABCDEFGHIJKLMNOPQRSTUVWX0123",
		},
		{
			name:   "clave de AWS",
			in:     "AKIAIOSFODNN7EXAMPLE en la configuración",
			want:   "*** en la configuración",
			secret: "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:   "clave de API estilo sk-",
			in:     "llamando con sk-ant-api03-abcdefghijklmnopqrstuv",
			want:   "llamando con ***",
			secret: "sk-ant-api03-abcdefghijklmnopqrstuv",
		},
		{
			name:   "credencial de un agent de status-sv",
			in:     "agent con svagt_0123456789abcdefghij",
			want:   "agent con ***",
			secret: "svagt_0123456789abcdefghij",
		},
		{
			name:   "JWT",
			in:     "sesión eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
			want:   "sesión ***",
			secret: "dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		},
		{
			name:   "clave privada",
			in:     "leí\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjE\n-----END OPENSSH PRIVATE KEY-----\nlisto",
			want:   "leí\n***\nlisto",
			secret: "b3BlbnNzaC1rZXktdjE",
		},
		{
			name:   "clave privada cortada al final del texto",
			in:     "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA",
			want:   "***",
			secret: "MIIEpAIBAAKCAQEA",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := String(tt.in)
			if strings.Contains(got, tt.secret) {
				t.Fatalf("el secreto sobrevivió: %q", got)
			}
			if tt.want != "" && got != tt.want {
				t.Errorf("\n obtuve   %q\n esperaba %q", got, tt.want)
			}
		})
	}
}

// Lo que no tiene secretos pasa igual: el enmascarado no puede volver ilegible
// una línea de comando común.
func TestLeavesOrdinaryTextAlone(t *testing.T) {
	for _, s := range []string{
		"/srv/app/.venv/bin/gunicorn app:wsgi --bind 127.0.0.1:8000 --workers 3",
		"nginx: worker process",
		"python3 manage.py runserver 0.0.0.0:8000",
		"https://cristales.example.com/health 200",
		"node /srv/web/server.js -p 3000",
		"2026-09-27T12:00:00Z INFO pedido atendido en 12 ms",
	} {
		if got := String(s); got != s {
			t.Errorf("se modificó texto sin secretos:\n antes   %q\n después %q", s, got)
		}
	}
}

func TestIsIdempotent(t *testing.T) {
	in := "app --password=x DB_TOKEN=y postgres://u:p@h/db"
	once := String(in)
	if twice := String(once); twice != once {
		t.Errorf("enmascarar dos veces cambia el resultado:\n %q\n %q", once, twice)
	}
}

// Un texto grande armado para trabar una expresión regular tiene que
// procesarse rápido: RE2 es lineal.
func TestLargeHostileInput(t *testing.T) {
	in := strings.Repeat("password= password= ", 20000) + strings.Repeat("a", 100000)
	if got := String(in); len(got) == 0 {
		t.Fatal("resultado vacío")
	}
}
