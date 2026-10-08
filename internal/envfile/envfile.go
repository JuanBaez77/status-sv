// Package envfile lee y escribe los archivos de variables de un proyecto
// (ADR-022): el .env que lee docker compose y el EnvironmentFile de systemd.
//
// Lo escribe `statussv-agent project accept` (y `project env`) con lo que una
// persona tipea en el servidor. Lo leen el agent (el .env de un proyecto
// Docker) y el ayudante root (el de systemd) solo para saber qué nombres
// faltan: los valores no salen de este paquete hacia ningún lado.
//
// Se escribe un formato que Compose y systemd leen igual: NOMBRE='valor', con
// comillas simples, adentro de las cuales ninguno de los dos interpreta nada
// (ni variables, ni escapes). Por eso un valor no puede llevar comillas
// simples ni saltos de línea.
package envfile

import (
	"bufio"
	"bytes"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/JuanBaez77/status-sv/proto"
)

// MaxValueLen acota un valor.
const MaxValueLen = 4096

// Var es una variable del archivo.
type Var struct {
	Name  string
	Value string
}

// Parse lee un archivo de variables: un NOMBRE=valor por renglón, con los
// comentarios y los renglones vacíos salteados, y "export " adelante
// tolerado. Si el valor va entre comillas (simples o dobles), se las saca. Un
// renglón que no se entiende se saltea: el archivo lo pudo escribir una
// persona a mano.
func Parse(data []byte) []Var {
	var vars []Var
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '\'' || value[0] == '"') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		vars = append(vars, Var{Name: name, Value: value})
	}
	return vars
}

// Missing devuelve los nombres de required que el archivo no tiene, o tiene
// vacíos, en el orden de required.
func Missing(required []string, data []byte) []string {
	present := make(map[string]bool)
	for _, v := range Parse(data) {
		present[v.Name] = v.Value != ""
	}
	var missing []string
	for _, name := range required {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// ErrBadValue es un valor que no se puede escribir.
var ErrBadValue = fmt.Errorf("un valor no puede tener comillas simples, saltos de línea ni caracteres de control, ni pasar de %d bytes", MaxValueLen)

// CheckValue dice si un valor se puede escribir.
func CheckValue(value string) error {
	if len(value) > MaxValueLen || strings.ContainsRune(value, '\'') {
		return ErrBadValue
	}
	for _, r := range value {
		if unicode.IsControl(r) && r != '\t' {
			return ErrBadValue
		}
	}
	return nil
}

// Format escribe las variables, en orden, una por renglón.
func Format(vars []Var) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("# Lo escribe statussv-agent project accept o project env (ADR-022).\n")
	seen := make(map[string]bool, len(vars))
	for _, v := range vars {
		if !proto.ValidEnvName(v.Name) {
			return nil, fmt.Errorf("%q no es un nombre de variable válido", v.Name)
		}
		if seen[v.Name] {
			return nil, fmt.Errorf("%s está repetida", v.Name)
		}
		seen[v.Name] = true
		if err := CheckValue(v.Value); err != nil {
			return nil, fmt.Errorf("%s: %w", v.Name, err)
		}
		fmt.Fprintf(&b, "%s='%s'\n", v.Name, v.Value)
	}
	return b.Bytes(), nil
}

// Merge devuelve las variables de current con los valores de updates encima.
// Las que solo están en updates van al final, en su orden. Las de current
// que no se pueden escribir con este formato (las escribió alguien a mano)
// son un error: se avisa en vez de perderlas.
func Merge(current, updates []Var) ([]Var, error) {
	out := slices.Clone(current)
	for _, v := range out {
		if !proto.ValidEnvName(v.Name) {
			return nil, fmt.Errorf("el archivo tiene %q, que no es un nombre que se pueda reescribir: editalo a mano", v.Name)
		}
	}
	for _, u := range updates {
		i := slices.IndexFunc(out, func(v Var) bool { return v.Name == u.Name })
		if i >= 0 {
			out[i].Value = u.Value
			continue
		}
		out = append(out, u)
	}
	return out, nil
}
