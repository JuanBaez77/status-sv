package tokens

import (
	"fmt"
	"slices"
	"strings"
)

// Capability es lo que un scope deja hacer.
type Capability string

// Capacidades: las de lectura del Sprint 7 y las de escritura del Sprint 8.
// Una de escritura deja pedir, no ejecutar: cada pedido espera la aprobación
// de una persona en el panel (ADR-026).
const (
	// CapRead son las herramientas de lectura sobre los proyectos y sus
	// deploys, y las de servidores.
	CapRead Capability = "read"
	// CapLogs es get_logs: va aparte porque los logs son lo más sensible que
	// el MCP puede mostrar.
	CapLogs Capability = "logs"
	// CapDeploy es pedir un deploy de un commit, o el primer deploy de un
	// proyecto aceptado.
	CapDeploy Capability = "deploy"
	// CapRollback es pedir volver al último estado bueno.
	CapRollback Capability = "rollback"
	// CapRestart es pedir reiniciar las unidades de un proyecto.
	CapRestart Capability = "restart"
)

// Entornos de un proyecto, los mismos del project.yaml.
const (
	EnvStaging    = "staging"
	EnvProduction = "production"
)

// Scope es un permiso: una capacidad sobre un entorno, como "read:staging".
type Scope string

// Scopes que existen, en el orden en que se guardan. La migración 00015
// rechaza cualquier otro.
var knownScopes = []Scope{
	"read:staging",
	"read:production",
	"logs:staging",
	"logs:production",
	"deploy:staging",
	"deploy:production",
	"rollback:staging",
	"rollback:production",
	"restart:staging",
	"restart:production",
}

// Writes dice si el scope deja pedir una escritura.
func (s Scope) Writes() bool {
	switch s.Capability() {
	case CapDeploy, CapRollback, CapRestart:
		return true
	}
	return false
}

// KnownScopes devuelve la lista cerrada de scopes, en su orden.
func KnownScopes() []Scope {
	return slices.Clone(knownScopes)
}

// Capability devuelve la capacidad del scope.
func (s Scope) Capability() Capability {
	capability, _, _ := strings.Cut(string(s), ":")
	return Capability(capability)
}

// Environment devuelve el entorno del scope.
func (s Scope) Environment() string {
	_, env, _ := strings.Cut(string(s), ":")
	return env
}

// ParseScopes valida una lista de scopes y la devuelve sin repetidos y en el
// orden de knownScopes, que es como se guarda.
func ParseScopes(raw []string) ([]Scope, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: el token necesita al menos un permiso", ErrInvalid)
	}
	wanted := make(map[Scope]bool, len(raw))
	for _, value := range raw {
		scope := Scope(value)
		if !slices.Contains(knownScopes, scope) {
			return nil, fmt.Errorf("%w: el permiso %q no existe", ErrInvalid, value)
		}
		wanted[scope] = true
	}
	scopes := make([]Scope, 0, len(wanted))
	for _, scope := range knownScopes {
		if wanted[scope] {
			scopes = append(scopes, scope)
		}
	}
	return scopes, nil
}

func scopeStrings(scopes []Scope) []string {
	out := make([]string, len(scopes))
	for i, scope := range scopes {
		out[i] = string(scope)
	}
	return out
}
