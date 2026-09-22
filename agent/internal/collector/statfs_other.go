//go:build !linux

package collector

import "errors"

// statfs fuera de Linux no existe: el agent solo corre en Linux, y en otros
// sistemas el paquete se compila para poder desarrollar y correr los tests.
func statfs(string) (fsUsage, error) {
	return fsUsage{}, errors.New("medir discos solo está implementado en Linux")
}
