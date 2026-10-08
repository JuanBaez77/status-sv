package accept

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/JuanBaez77/status-sv/internal/envfile"
	"github.com/JuanBaez77/status-sv/internal/projectspec"
	"github.com/JuanBaez77/status-sv/proto"
)

// Los valores de las variables requeridas se cargan acá, en el servidor, y no
// salen de él (ADR-022, punto 4). Con Docker van al .env del workdir, que lee
// docker compose como el agent; con systemd, al archivo de entorno del
// proyecto, que es solo de root y lo lee systemd.

// maxEnvFileBytes acota lo que se lee de un archivo de variables.
const maxEnvFileBytes = 1 << 20

// maxTries es cuántas veces se pide un valor que no se puede escribir.
const maxTries = 3

// envTarget es dónde quedan los valores de un proyecto y con qué permisos.
type envTarget struct {
	path  string
	mode  os.FileMode
	group string
	// dir es el directorio que hay que crear, de root y solo de root, si el
	// archivo va en /etc/status-sv/env.
	dir string
}

// envTargetFor dice dónde van los valores. Con systemd siempre hay archivo
// (una propuesta lo declara); con Docker, solo si hay variables requeridas.
func (a *Accepter) envTargetFor(spec projectspec.Spec) (envTarget, bool) {
	if spec.Project.Runtime == proto.RuntimeSystemd {
		if spec.EnvFile == "" {
			return envTarget{}, false
		}
		path := a.onDisk(spec.EnvFile)
		return envTarget{path: path, mode: 0o600, group: "root", dir: filepath.Dir(path)}, true
	}
	if len(spec.RequiredEnv) == 0 {
		return envTarget{}, false
	}
	return envTarget{path: filepath.Join(a.onDisk(spec.Project.Workdir), ".env"), mode: 0o640, group: AgentUser}, true
}

// secretsStep es el paso del alta que carga los valores.
func (a *Accepter) secretsStep(acc *acceptance) (action, bool) {
	target, ok := a.envTargetFor(acc.spec)
	if !ok {
		return action{}, false
	}
	what := fmt.Sprintf("cargar las variables requeridas (%s) en %s, sin mostrarlas", strings.Join(acc.spec.RequiredEnv, ", "), acc.envShown())
	if len(acc.spec.RequiredEnv) == 0 {
		what = fmt.Sprintf("crear el archivo de entorno %s (el proyecto no declara variables requeridas)", acc.envShown())
	}
	return action{what: what, apply: func(context.Context) error {
		return a.writeSecrets(acc.spec, target, acc.opts.EnvFile, false)
	}}, true
}

// envShown es la ruta del archivo de variables como se ve en el servidor.
func (acc *acceptance) envShown() string {
	if acc.spec.Project.Runtime == proto.RuntimeSystemd {
		return acc.spec.EnvFile
	}
	return acc.spec.Project.Workdir + "/.env"
}

// writeSecrets pide los valores que faltan (o, con fromFile, los toma de ese
// archivo), los suma a lo que ya había y escribe el archivo. Con all, pide
// también los que ya están, y Enter deja el que había.
func (a *Accepter) writeSecrets(spec projectspec.Spec, target envTarget, fromFile string, all bool) error {
	current, err := readEnvFile(target.path)
	if err != nil {
		return err
	}
	var updates []envfile.Var
	if fromFile != "" {
		data, err := readLimited(fromFile)
		if err != nil {
			return fmt.Errorf("leer %s: %w", fromFile, err)
		}
		updates = envfile.Parse(data)
	} else {
		have := make(map[string]bool, len(current))
		for _, v := range current {
			have[v.Name] = v.Value != ""
		}
		for _, name := range spec.RequiredEnv {
			if have[name] && !all {
				a.printf("%s: ya está cargada\n", name)
				continue
			}
			value, err := a.askValue(name, have[name])
			if err != nil {
				return err
			}
			if value == "" {
				continue
			}
			updates = append(updates, envfile.Var{Name: name, Value: value})
		}
	}

	merged, err := envfile.Merge(current, updates)
	if err != nil {
		return err
	}
	data, err := envfile.Format(merged)
	if err != nil {
		return err
	}
	if target.dir != "" {
		if err := a.makeRootDir(target.dir); err != nil {
			return err
		}
	}
	if err := a.writeRootFile(target.path, data, target.mode, target.group); err != nil {
		return err
	}
	if missing := envfile.Missing(spec.RequiredEnv, data); len(missing) > 0 {
		a.printf("aviso: faltan %s; el primer deploy no arranca hasta cargarlas con:\n  sudo statussv-agent project env %s\n",
			strings.Join(missing, ", "), spec.Project.Name)
	}
	return nil
}

// askValue pide el valor de una variable sin mostrarlo, hasta maxTries veces
// si no se puede escribir. Vacío deja la variable como estaba.
func (a *Accepter) askValue(name string, has bool) (string, error) {
	prompt := name + ": "
	if has {
		prompt = name + " (ya cargada; Enter la deja como está): "
	}
	for range maxTries {
		value, err := a.secret(prompt)
		if err != nil {
			return "", err
		}
		if err := envfile.CheckValue(value); err != nil {
			a.printf("%v\n", err)
			continue
		}
		return value, nil
	}
	return "", fmt.Errorf("%s: no se pudo cargar un valor válido", name)
}

// secret lee un valor sin eco si In es una terminal; si no (un script, la
// prueba del CI), un renglón.
func (a *Accepter) secret(prompt string) (string, error) {
	if a.ReadSecret != nil {
		return a.ReadSecret(prompt)
	}
	if f, ok := a.In.(*os.File); ok && term.IsTerminal(int(f.Fd())) { //nolint:gosec // un descriptor siempre entra en int
		a.printf("%s", prompt)
		value, err := term.ReadPassword(int(f.Fd())) //nolint:gosec // ídem
		a.printf("\n")
		if err != nil {
			return "", fmt.Errorf("leer el valor: %w", err)
		}
		return string(value), nil
	}
	a.printf("%s", prompt)
	line, err := a.reader().ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readEnvFile lee las variables que ya hay. No sigue enlaces simbólicos: el
// .env de un proyecto Docker vive en un directorio donde escribe el grupo del
// agent, y root no tiene que leer otro archivo por ese camino.
func readEnvFile(path string) ([]envfile.Var, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s no es un archivo regular: no se toca", path)
	}
	data, err := readLimited(path)
	if err != nil {
		return nil, err
	}
	return envfile.Parse(data), nil
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // la ruta la da quien corre el comando, o sale de la declaración
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxEnvFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxEnvFileBytes {
		return nil, fmt.Errorf("%s pesa más de %d bytes", path, maxEnvFileBytes)
	}
	return data, nil
}

// makeRootDir crea un directorio de root que nadie más lee (0700).
func (a *Accepter) makeRootDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := a.Sys.Chown(dir, "root", "root"); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700) //nolint:gosec // un directorio necesita el bit de ejecución
}

// Env carga o cambia los valores de las variables requeridas de un proyecto
// ya declarado: `sudo statussv-agent project env <nombre>`. Pide cada una;
// Enter deja la que había.
func (a *Accepter) Env(_ context.Context, name, fromFile string) error {
	if !a.Sys.IsRoot() {
		return ErrNotRoot
	}
	if !proto.ValidProjectName(name) {
		return errors.New("nombre de proyecto inválido")
	}
	if a.LoadSpec == nil {
		return errors.New("falta con qué leer la declaración")
	}
	spec, err := a.LoadSpec(a.Paths.Projects, name)
	if err != nil {
		return fmt.Errorf("no se pudo leer la declaración de %s: %w", name, err)
	}
	target, ok := a.envTargetFor(spec)
	if !ok {
		return fmt.Errorf("%s no declara variables requeridas ni un archivo de entorno", name)
	}
	if len(spec.RequiredEnv) == 0 && fromFile == "" {
		return fmt.Errorf("%s no declara variables requeridas: para cargar otras, usá --env-file", name)
	}
	a.printf("Variables de %s, en %s:\n", name, target.path)
	if err := a.writeSecrets(spec, target, fromFile, true); err != nil {
		return err
	}
	a.printf("Listo. El servicio toma los valores nuevos cuando se reinicia: con el próximo deploy, o reiniciándolo a mano.\n")
	return nil
}
