package unithelper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// El ayudante corre como root lo que dice el archivo: si no es de root, o si
// alguien más lo puede escribir, no lo usa.
func TestLoadTrustedNeedsRootOwnedFiles(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("hace falta ser root para crear archivos de root (en CI corre con sudo o en el contenedor)")
	}
	dir := filepath.Join(t.TempDir(), "projects.d")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(facturasYAML, "ENVFILE", "/etc/facturas/env", 1)
	file := filepath.Join(dir, "facturas.yaml")
	if err := os.WriteFile(file, []byte(yaml), 0o640); err != nil {
		t.Fatal(err)
	}

	spec, err := LoadTrusted(dir, "facturas")
	if err != nil {
		t.Fatalf("LoadTrusted: %v", err)
	}
	if spec.RunAs != "facturas" || len(spec.Build) != 2 {
		t.Errorf("declaración = %+v", spec)
	}

	// Escribible por el grupo: el agent, que es del grupo, podría cambiarlo.
	if err := os.Chmod(file, 0o660); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrusted(dir, "facturas"); err == nil {
		t.Error("se aceptó un archivo que el grupo puede escribir")
	}
	if err := os.Chmod(file, 0o640); err != nil {
		t.Fatal(err)
	}

	// De otro dueño.
	if err := os.Chown(file, 65534, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrusted(dir, "facturas"); err == nil {
		t.Error("se aceptó un archivo que no es de root")
	}
	if err := os.Chown(file, 0, 0); err != nil {
		t.Fatal(err)
	}

	// El directorio escribible por el grupo: se podría reemplazar el archivo.
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrusted(dir, "facturas"); err == nil {
		t.Error("se aceptó un directorio que el grupo puede escribir")
	}
}
