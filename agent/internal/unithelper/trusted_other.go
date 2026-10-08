//go:build !linux

package unithelper

import (
	"errors"

	"github.com/JuanBaez77/status-sv/internal/projectspec"
)

// LoadTrusted solo existe en Linux, que es donde corre el ayudante.
func LoadTrusted(string, string) (projectspec.Spec, error) {
	return projectspec.Spec{}, errors.New("el ayudante solo corre en Linux")
}
