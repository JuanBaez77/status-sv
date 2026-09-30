//go:build unix

package compose

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/docker"
	"github.com/JuanBaez77/status-sv/agent/internal/projectspec"
	"github.com/JuanBaez77/status-sv/agent/internal/runner"
	"github.com/JuanBaez77/status-sv/proto"
)

// TestWithRealDocker recorre lo que hace el motor contra un Docker de verdad:
// compilar, correr un paso, levantar, anotar y guardar la imagen, compilar
// otra versión y volver a la anterior sin recompilar.
//
// Necesita Docker con el plugin de Compose y baja busybox, así que corre solo
// con STATUSSV_TEST_DOCKER=1 (el job de Go del CI lo prende).
func TestWithRealDocker(t *testing.T) {
	if os.Getenv("STATUSSV_TEST_DOCKER") != "1" {
		t.Skip("STATUSSV_TEST_DOCKER no está en 1")
	}
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal("STATUSSV_TEST_DOCKER=1 pero no hay docker")
	}
	socket := "/var/run/docker.sock"
	api := docker.NewClient(socket)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	workdir := t.TempDir()
	name := "statussv-prueba-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	write := func(file, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(workdir, file), []byte(content), 0o644); err != nil { //nolint:gosec // archivo de prueba
			t.Fatal(err)
		}
	}
	write("Dockerfile", "FROM busybox:1.37\nCOPY version /version\nCMD [\"sh\", \"-c\", \"cat /version; sleep 3600\"]\n")
	write("compose.yml", "services:\n  web:\n    build: .\n    init: true\n    healthcheck:\n      test: [\"CMD\", \"test\", \"-f\", \"/version\"]\n      interval: 1s\n      retries: 30\n")
	write("version", "v1\n")

	state := t.TempDir()
	p, err := New(Config{Docker: dockerPath, Socket: socket, StateDir: state}, proto.Project{
		Name: "prueba", Environment: proto.EnvStaging, Repo: "github.com/a/b", Branch: "main",
		Runtime: proto.RuntimeDocker, Workdir: workdir, ComposeFile: "compose.yml", ComposeProject: name,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		down := p.command(time.Minute, "down", "--rmi", "local", "--volumes")
		_ = runner.Run(context.Background(), down, nil)
		refs, _ := api.ImagesByReference(context.Background(), RollbackRepoPrefix+name+"/*")
		for _, ref := range refs {
			_ = api.Untag(context.Background(), ref)
		}
	})

	tail := runner.NewTail(20, 200)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v\n%s", what, err, strings.Join(tail.Lines(), "\n"))
		}
	}

	version, err := p.Version(ctx)
	must("versión", err)
	t.Logf("compose %s", version)
	must("config", p.Validate(ctx, tail))
	must("build v1", p.Build(ctx, tail))

	// Un paso declarado corre con la imagen recién compilada.
	stepTail := runner.NewTail(5, 200)
	must("paso", p.Run(ctx, projectspec.Step{Service: "web", Command: []string{"cat", "/version"}, Timeout: time.Minute}, stepTail))
	if !strings.Contains(strings.Join(stepTail.Lines(), "|"), "v1") {
		t.Errorf("el paso no mostró v1: %q", stepTail.Lines())
	}
	failing := p.Run(ctx, projectspec.Step{Service: "web", Command: []string{"false"}, Timeout: time.Minute}, nil)
	var exit *runner.ExitError
	if !errors.As(failing, &exit) {
		t.Errorf("un paso que falla devolvió %v", failing)
	}

	must("up v1", p.Up(ctx, tail))
	waitReady(ctx, t, p, api)

	before, err := p.Snapshot(ctx, api)
	must("snapshot", err)
	if len(before) != 1 || before[0].Service != "web" || !strings.HasPrefix(before[0].ID, "sha256:") {
		t.Fatalf("snapshot = %+v", before)
	}
	const commitV1 = "1111111111111111111111111111111111111111"
	must("retain", p.Retain(ctx, api, commitV1, before))

	// La versión nueva: compila y levanta con otra imagen.
	write("version", "v2\n")
	must("build v2", p.Build(ctx, tail))
	must("up v2", p.Up(ctx, tail))
	waitReady(ctx, t, p, api)
	after, err := p.Snapshot(ctx, api)
	must("snapshot v2", err)
	if after[0].ID == before[0].ID {
		t.Fatal("la versión nueva corre con la misma imagen")
	}

	// Volver atrás: la referencia vuelve a la imagen anotada y up recrea.
	must("restore", p.Restore(ctx, api, before))
	must("up de vuelta", p.Up(ctx, tail))
	waitReady(ctx, t, p, api)
	back, err := p.Snapshot(ctx, api)
	must("snapshot de vuelta", err)
	if back[0].ID != before[0].ID {
		t.Fatalf("después de volver corre %s y se esperaba %s", back[0].ID, before[0].ID)
	}

	// Sacar las etiquetas que no hay que conservar.
	must("prune", p.Prune(ctx, api, nil))
	if refs, _ := api.ImagesByReference(ctx, RollbackRepoPrefix+name+"/*"); len(refs) != 0 {
		t.Errorf("quedaron etiquetas: %v", refs)
	}
}

func waitReady(ctx context.Context, t *testing.T, p *Project, api ImageAPI) {
	t.Helper()
	var status Status
	for range 60 {
		var err error
		status, err = p.Check(ctx, api)
		if err != nil {
			t.Fatal(err)
		}
		if status.Ready {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("el proyecto no quedó listo: %s", status.Detail)
}
