package unithelper

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/JuanBaez77/status-sv/agent/internal/projects"
	"github.com/JuanBaez77/status-sv/internal/projectspec"
)

// LoadTrusted lee la declaración de un proyecto como projects.ReadSpec, pero
// antes exige que el directorio y el archivo sean de root y que nadie más los
// pueda escribir.
//
// El ayudante corre como root con lo que diga ese archivo. Si por un error de
// permisos el agent pudiera escribirlo, podría declararse sus propios pasos:
// en ese caso el ayudante no hace nada.
func LoadTrusted(dir, name string) (projectspec.Spec, error) {
	if err := rootOnly(dir); err != nil {
		return projectspec.Spec{}, err
	}
	for _, ext := range []string{".yaml", ".yml"} {
		path := filepath.Join(dir, name+ext)
		if _, err := os.Lstat(path); err != nil {
			continue
		}
		if err := rootOnly(path); err != nil {
			return projectspec.Spec{}, err
		}
	}
	return projects.ReadSpec(dir, name)
}

// rootOnly dice si path es de root y solo root lo puede escribir.
func rootOnly(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("no se puede mirar %s: %w", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("no se pudo leer el dueño del archivo")
	}
	if stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s tiene que ser de root y sin escritura para el grupo ni para otros", path)
	}
	return nil
}

// RootOnly dice si path es de root y solo root lo puede escribir. Lo usan
// también las plantillas del servidor (ADR-023): una que otro puede escribir
// no se usa.
func RootOnly(path string) error {
	return rootOnly(path)
}
