package accept

import "os"

// fchown no hace nada en Windows, donde corren algunas pruebas: el comando
// corre en un servidor Linux.
func fchown(*os.File, int, int) error {
	return nil
}
