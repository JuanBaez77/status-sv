// Package state guarda en disco lo que el agent necesita recordar entre
// arranques: su credencial y a qué control plane pertenece.
//
// El archivo tiene permisos 0600 y su directorio 0700, porque adentro está la
// credencial en claro: es el único lugar del sistema donde existe.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// Permisos del archivo de estado y de su directorio. En Linux, que es donde
// corre el agent, esto deja el secreto accesible solo para su usuario.
const (
	FileMode = 0o600
	DirMode  = 0o700
)

// ErrNotEnrolled es que todavía no hay estado guardado.
var ErrNotEnrolled = errors.New("el agent no está enrolado")

// State es lo que el agent recuerda entre arranques.
type State struct {
	// ServerURL es el control plane al que pertenece este agent.
	ServerURL string `json:"server_url"`
	// ServerID y ServerName los asigna el control plane al enrolar.
	ServerID   uuid.UUID `json:"server_id"`
	ServerName string    `json:"server_name"`
	// Credential es la credencial propia del agent, en claro. No sale de este
	// archivo salvo en el encabezado Authorization de la conexión.
	Credential string `json:"credential"`
	// EnrolledAt es cuándo se enroló, para poder mirarlo al depurar.
	EnrolledAt time.Time `json:"enrolled_at"`
}

// Validate revisa que el estado sirva para conectarse.
func (s State) Validate() error {
	switch {
	case s.ServerURL == "":
		return errors.New("falta la dirección del control plane")
	case s.ServerID == uuid.Nil:
		return errors.New("falta el ID del servidor")
	case s.ServerName == "":
		return errors.New("falta el nombre del servidor")
	case s.Credential == "":
		return errors.New("falta la credencial")
	}
	return nil
}

// Load lee el estado del disco.
func Load(path string) (State, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // la ruta la elige quien opera el agent
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, fmt.Errorf("%w: no existe %s", ErrNotEnrolled, path)
		}
		return State{}, fmt.Errorf("leer el estado: %w", err)
	}

	var loaded State
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return State{}, fmt.Errorf("leer el estado de %s: %w", path, err)
	}
	if err := loaded.Validate(); err != nil {
		return State{}, fmt.Errorf("el estado de %s está incompleto: %w", path, err)
	}
	return loaded, nil
}

// Save escribe el estado con permisos restrictivos.
//
// Escribe primero en un archivo temporal y después lo mueve encima del
// definitivo: si el proceso muere a mitad de camino, el estado anterior queda
// intacto en vez de convertirse en un archivo a medio escribir.
func Save(path string, s State) error {
	if err := s.Validate(); err != nil {
		return fmt.Errorf("no se guarda un estado inválido: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("crear el directorio %s: %w", dir, err)
	}

	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("serializar el estado: %w", err)
	}
	raw = append(raw, '\n')

	temp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("crear el archivo temporal: %w", err)
	}
	tempPath := temp.Name()
	// Si algo falla de acá en adelante, no se deja basura en el directorio.
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()

	// Los permisos se ajustan antes de escribir el secreto, no después: así no
	// hay un instante en que el archivo exista con la credencial y permisos
	// más abiertos.
	if err := temp.Chmod(FileMode); err != nil {
		return fmt.Errorf("ajustar los permisos: %w", err)
	}
	if _, err := temp.Write(raw); err != nil {
		return fmt.Errorf("escribir el estado: %w", err)
	}
	// Sin esto, un corte de luz justo después del rename puede dejar el archivo
	// vacío: el nombre se renombra pero el contenido todavía está en el caché.
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sincronizar el estado: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("cerrar el archivo temporal: %w", err)
	}

	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("mover el estado a %s: %w", path, err)
	}
	return nil
}

// Exists dice si ya hay un estado guardado.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
