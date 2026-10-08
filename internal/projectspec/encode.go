package projectspec

import (
	"bytes"
	"fmt"

	"go.yaml.in/yaml/v3"
)

// Declaration es un project.yaml para escribir: lo arma el control plane con
// la propuesta de un proyecto nuevo (ADR-022). Se escribe con Encode, que lo
// vuelve a leer con ParseSpec: lo que sale es siempre un archivo que el
// parser estricto acepta.
type Declaration struct {
	Name        string               `yaml:"name"`
	Environment string               `yaml:"environment"`
	Repo        string               `yaml:"repo"`
	Branch      string               `yaml:"branch"`
	Runtime     string               `yaml:"runtime"`
	ComposeFile string               `yaml:"compose_file,omitempty"`
	Workdir     string               `yaml:"workdir"`
	RunAs       string               `yaml:"run_as,omitempty"`
	EnvFile     string               `yaml:"env_file,omitempty"`
	RequiredEnv []string             `yaml:"required_env,omitempty,flow"`
	Service     *DeclaredService     `yaml:"service,omitempty"`
	Site        *DeclaredSite        `yaml:"site,omitempty"`
	Healthcheck *DeclaredHealthcheck `yaml:"healthcheck,omitempty"`
	Deploy      *DeclaredSteps       `yaml:"deploy,omitempty"`
}

// DeclaredService es el servicio de la unidad generada.
type DeclaredService struct {
	Command []string `yaml:"command,flow"`
}

// DeclaredSite es el sitio de nginx.
type DeclaredSite struct {
	Domain string `yaml:"domain"`
	Port   int    `yaml:"port"`
}

// DeclaredHealthcheck es el healthcheck.
type DeclaredHealthcheck struct {
	URL            string `yaml:"url"`
	ExpectStatus   int    `yaml:"expect_status,omitempty"`
	TimeoutSeconds int    `yaml:"timeout_seconds,omitempty"`
}

// DeclaredSteps son los pasos declarados.
type DeclaredSteps struct {
	Build []DeclaredStep `yaml:"build,omitempty"`
	Pre   []DeclaredStep `yaml:"pre,omitempty"`
	Post  []DeclaredStep `yaml:"post,omitempty"`
}

// DeclaredStep es un paso declarado.
type DeclaredStep struct {
	Service        string   `yaml:"service,omitempty"`
	Command        []string `yaml:"command,flow"`
	TimeoutSeconds int      `yaml:"timeout_seconds,omitempty"`
}

// Encode escribe la declaración como project.yaml y la valida con ParseSpec.
// Devuelve el texto y lo que leyó el parser.
func Encode(d Declaration) ([]byte, Spec, error) {
	if d.Deploy != nil && len(d.Deploy.Build)+len(d.Deploy.Pre)+len(d.Deploy.Post) == 0 {
		d.Deploy = nil
	}
	var b bytes.Buffer
	encoder := yaml.NewEncoder(&b)
	encoder.SetIndent(2)
	if err := encoder.Encode(d); err != nil {
		return nil, Spec{}, fmt.Errorf("escribir la declaración: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, Spec{}, fmt.Errorf("escribir la declaración: %w", err)
	}
	data := b.Bytes()
	spec, err := ParseSpec(d.Name+".yaml", data)
	if err != nil {
		return nil, Spec{}, err
	}
	return data, spec, nil
}
