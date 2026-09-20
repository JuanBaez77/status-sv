// Package buildinfo expone la identidad del binario: versión, commit y fecha de
// compilación. Lo comparten el agent y el control plane para responder --version
// y para reportar su versión en el latido.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Valores inyectados al compilar con -ldflags "-X github.com/JuanBaez77/status-sv/internal/buildinfo.Version=...".
// Si no se inyectan, se usa la información que Go embebe desde el repositorio.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

const unknown = "desconocido"

// Line devuelve una línea legible con la identidad del binario.
func Line(name string) string {
	return fmt.Sprintf("%s %s (commit %s, compilado %s, %s/%s, %s)",
		name, Version, ShortCommit(), BuildDate(), runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// ShortCommit devuelve los primeros siete caracteres del commit, o el commit que
// Go embebe automáticamente si no se inyectó ninguno.
func ShortCommit() string {
	commit := Commit
	if commit == "" {
		commit = vcsRevision()
	}
	if commit == "" {
		return unknown
	}
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}

// BuildDate devuelve la fecha de compilación inyectada, si la hay.
func BuildDate() string {
	if Date == "" {
		return unknown
	}
	return Date
}

// vcsRevision lee el commit que el compilador de Go embebe cuando el build sale
// de un repositorio git.
func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return setting.Value
		}
	}
	return ""
}
