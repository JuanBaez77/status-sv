//go:build integration

package testdb

import "os"

// envTestDatabaseURL está aparte para que quede en un solo lugar el nombre de la
// variable que apunta a la base de pruebas.
func envTestDatabaseURL() string {
	return os.Getenv("TEST_DATABASE_URL")
}
