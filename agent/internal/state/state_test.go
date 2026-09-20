package state_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/agent/internal/state"
)

func sample() state.State {
	return state.State{
		ServerURL:  "https://status.example.com",
		ServerID:   uuid.MustParse("01924f9c-0000-7000-8000-000000000001"),
		ServerName: "vps-01",
		Credential: "svagt_abcdefghijklmnopqrstuvwxyz0123456789",
		EnrolledAt: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status-sv", "agent.json")

	original := sample()
	if err := state.Save(path, original); err != nil {
		t.Fatalf("guardar el estado: %v", err)
	}

	loaded, err := state.Load(path)
	if err != nil {
		t.Fatalf("cargar el estado: %v", err)
	}
	if loaded != original {
		t.Errorf("el estado cambió al ir y volver:\n  antes: %+v\n  después: %+v", original, loaded)
	}
}

// El archivo tiene la credencial en claro: nadie más que su dueño puede leerlo.
func TestSavedFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("los permisos POSIX no aplican en Windows; el agent corre en Linux")
	}

	dir := filepath.Join(t.TempDir(), "status-sv")
	path := filepath.Join(dir, "agent.json")

	if err := state.Save(path, sample()); err != nil {
		t.Fatalf("guardar el estado: %v", err)
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("consultar el archivo: %v", err)
	}
	if mode := fileInfo.Mode().Perm(); mode != state.FileMode {
		t.Errorf("el archivo quedó con permisos %o, se esperaba %o", mode, state.FileMode)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("consultar el directorio: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != state.DirMode {
		t.Errorf("el directorio quedó con permisos %o, se esperaba %o", mode, state.DirMode)
	}
}

func TestLoadWithoutStateSaysNotEnrolled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-existe.json")

	_, err := state.Load(path)
	if !errors.Is(err, state.ErrNotEnrolled) {
		t.Fatalf("se esperaba ErrNotEnrolled, se obtuvo %v", err)
	}
	if state.Exists(path) {
		t.Error("Exists dijo que sí sobre un archivo que no está")
	}
}

func TestLoadRejectsIncompleteState(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "no es JSON", content: "cualquier cosa", want: "leer el estado"},
		{name: "sin credencial", content: `{"server_url":"https://x","server_id":"01924f9c-0000-7000-8000-000000000001","server_name":"vps-01"}`, want: "credencial"},
		{name: "sin servidor", content: `{"server_url":"https://x","credential":"svagt_x"}`, want: "ID del servidor"},
		{name: "vacío", content: `{}`, want: "dirección del control plane"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.json")
			if err := os.WriteFile(path, []byte(tt.content), state.FileMode); err != nil {
				t.Fatalf("escribir el archivo de prueba: %v", err)
			}

			_, err := state.Load(path)
			if err == nil {
				t.Fatal("se cargó un estado incompleto")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("el error no menciona %q: %v", tt.want, err)
			}
		})
	}
}

func TestSaveRejectsInvalidState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")

	incomplete := sample()
	incomplete.Credential = ""

	if err := state.Save(path, incomplete); err == nil {
		t.Fatal("se guardó un estado sin credencial")
	}
	if state.Exists(path) {
		t.Error("se creó el archivo a pesar de que el estado era inválido")
	}
}

// Guardar dos veces no puede dejar el estado a medio escribir ni acumular
// archivos temporales.
func TestSaveOverwritesCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")

	if err := state.Save(path, sample()); err != nil {
		t.Fatalf("primer guardado: %v", err)
	}

	updated := sample()
	updated.Credential = "svagt_la-credencial-nueva-despues-de-rotar"
	if err := state.Save(path, updated); err != nil {
		t.Fatalf("segundo guardado: %v", err)
	}

	loaded, err := state.Load(path)
	if err != nil {
		t.Fatalf("cargar el estado: %v", err)
	}
	if loaded.Credential != updated.Credential {
		t.Errorf("quedó la credencial vieja: %q", loaded.Credential)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("listar el directorio: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("quedaron %d archivos en el directorio: %v", len(entries), names)
	}
}
