package main

import (
	"bytes"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/config"
	"github.com/JuanBaez77/status-sv/agent/internal/projectspec"
	"github.com/JuanBaez77/status-sv/proto"
)

func TestPrintDeployPaths(t *testing.T) {
	state := proto.ProjectsState{
		TakenAt: time.Now(),
		Source:  proto.RuntimeSource{Status: proto.SourceOK},
		Projects: []proto.Project{
			{Name: "tienda", Runtime: proto.RuntimeDocker, Workdir: "/srv/tienda"},
			{Name: "facturas", Runtime: proto.RuntimeSystemd, Workdir: "/srv/facturas"},
			{Name: "casa", Runtime: proto.RuntimeDocker, Workdir: "/home/juan/casa"},
			{Name: "raiz", Runtime: proto.RuntimeDocker, Workdir: "/root"},
		},
		Invalid: []proto.InvalidProject{{File: "roto.yaml", Error: "name: falta"}},
	}
	var out, warn bytes.Buffer
	if err := printDeployPaths(&out, &warn, state); err != nil {
		t.Fatal(err)
	}
	// Todos los que se pueden desplegar, con su runtime, y nada que ProtectHome
	// esconda.
	if out.String() != "tienda /srv/tienda docker\nfacturas /srv/facturas systemd\n" {
		t.Errorf("salida = %q", out.String())
	}
	for _, want := range []string{"casa", "raiz", "roto.yaml"} {
		if !strings.Contains(warn.String(), want) {
			t.Errorf("falta el aviso de %s: %q", want, warn.String())
		}
	}

	if err := printDeployPaths(&out, &warn, proto.ProjectsState{Source: proto.RuntimeSource{Status: proto.SourceError, Detail: "permiso denegado"}}); err == nil {
		t.Error("con el directorio ilegible tendría que fallar: el instalador no puede escribir un drop-in a ciegas")
	}
}

func TestHidden(t *testing.T) {
	for dir, want := range map[string]bool{
		"/srv/x": false, "/opt/app": false, "/home": true, "/home/juan/x": true, "/root": true, "/run/user/1000/x": true, "/homes/x": false,
	} {
		if hidden(dir) != want {
			t.Errorf("hidden(%q) = %v", dir, !want)
		}
	}
}

// Sin el ayudante instalado, deploy-check de un proyecto de systemd dice qué
// falta y cómo se arregla, en vez de un error de conexión.
func TestCheckSystemdWithoutHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("un socket que no existe da otro error en Windows; el agent corre en Linux")
	}
	var out bytes.Buffer
	c := &checker{out: &out}
	cfg := config.Config{UnitHelperSocket: filepath.Join(t.TempDir(), "no-hay-socket")}
	spec := projectspec.Spec{Project: proto.Project{Name: "facturas", Runtime: proto.RuntimeSystemd, Units: []string{"gunicorn-facturas.service"}}}

	c.systemd(t.Context(), cfg, spec)

	if c.failures != 1 || !strings.Contains(out.String(), "install-agent.sh --enable-deploy") {
		t.Errorf("fallas = %d, salida = %q", c.failures, out.String())
	}
}
