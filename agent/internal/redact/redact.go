// Package redact enmascara secretos en texto antes de que salga del servidor.
//
// Es la regla 3 del proyecto: los secretos no viajan ni al LLM ni al panel. El
// enmascarado se hace en el agent (ADR-011), así que ni un control plane
// comprometido los ve. Lo usan la línea de comando de los procesos y, más
// adelante, los logs.
//
// Es a propósito exagerado: tapar algo que no era secreto cuesta un dato en el
// panel; dejar pasar una contraseña no tiene vuelta atrás.
package redact

import "regexp"

// Mask es lo que queda en lugar de un secreto.
const Mask = "***"

// secretWords son las palabras que, en el nombre de una variable, de una
// opción o de una clave, dicen que su valor es un secreto.
const secretWords = `(?:password|passwd|passphrase|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|auth|credential|dsn)`

// rule es un patrón y cómo reemplazarlo. Todos son de RE2, que corre en tiempo
// lineal: un log armado a propósito no puede colgar al agent.
type rule struct {
	re   *regexp.Regexp
	repl string
}

var rules = []rule{
	// Bloques de clave privada, enteros.
	{
		re:   regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`),
		repl: Mask,
	},
	// Credenciales en una URL: postgres://usuario:clave@host.
	{
		re:   regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]*:)[^\s@/]+@`),
		repl: "${1}" + Mask + "@",
	},
	// Encabezados de autorización: "Authorization: Bearer x", "Basic x".
	{
		re:   regexp.MustCompile(`(?i)\b(bearer|basic)(\s+)[A-Za-z0-9._~+/=-]{8,}`),
		repl: "${1}${2}" + Mask,
	},
	// Clientes que aceptan la contraseña pegada a -p o después de -p, sin
	// ninguna palabra que la delate: mysql -pClave, sshpass -p clave.
	{
		re:   regexp.MustCompile(`(?i)(\b(?:mysql|mysqldump|mysqladmin|mariadb|mariadb-dump)\b[^\n]*?\s-p)([^\s-]\S*)`),
		repl: "${1}" + Mask,
	},
	{
		re:   regexp.MustCompile(`(?i)(\bsshpass\b[^\n]*?\s-p\s*)(\S+)`),
		repl: "${1}" + Mask,
	},
	// Opciones de línea de comando con el valor aparte o con igual:
	// --password x, --api-key=x, -token x.
	{
		re:   regexp.MustCompile(`(?i)(--?[a-z0-9_-]*` + secretWords + `[a-z0-9_-]*)(=|\s+)("[^"]*"|'[^']*'|\S+)`),
		repl: "${1}${2}" + Mask,
	},
	// Asignaciones: DB_PASSWORD=x, "api_key": "x", secret: x.
	{
		re:   regexp.MustCompile(`(?i)([a-z0-9_.-]*` + secretWords + `[a-z0-9_.-]*["']?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;&]+)`),
		repl: "${1}" + Mask,
	},
	// Formatos de token que se reconocen solos, sin una palabra al lado.
	{
		re: regexp.MustCompile(`\b(?:` +
			`gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|` + // GitHub
			`glpat-[A-Za-z0-9_-]{20,}|` + // GitLab
			`xox[abprs]-[A-Za-z0-9-]{10,}|` + // Slack
			`AKIA[0-9A-Z]{16}|` + // AWS
			`sk-[A-Za-z0-9_-]{20,}|` + // OpenAI, Anthropic y otros
			`sv(?:enr|agt|ses)_[A-Za-z0-9_-]{16,}|` + // los de status-sv
			`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+` + // JWT
			`)`),
		repl: Mask,
	},
}

// String devuelve s con los secretos reemplazados por Mask.
func String(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
