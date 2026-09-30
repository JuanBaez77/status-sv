package compose

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/JuanBaez77/status-sv/agent/internal/docker"
	"github.com/JuanBaez77/status-sv/agent/internal/projectspec"
	"github.com/JuanBaez77/status-sv/proto"
)

const (
	sha1 = "0123456789abcdef0123456789abcdef01234567"
	sha2 = "89abcdef0123456789abcdef0123456789abcdef"
	imgA = "sha256:aaaa"
	imgB = "sha256:bbbb"
)

func project() proto.Project {
	return proto.Project{
		Name: "tienda", Environment: proto.EnvStaging, Repo: "github.com/a/tienda", Branch: "main",
		Runtime: proto.RuntimeDocker, Workdir: "/srv/tienda", ComposeFile: "deploy/compose.yml", ComposeProject: "tienda",
	}
}

func newProject(t *testing.T) *Project {
	t.Helper()
	p, err := New(Config{Docker: "/usr/bin/docker", Socket: "/run/docker.sock", StateDir: "/var/lib/status-sv"}, project())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeAPI es la API de Docker en memoria.
type fakeAPI struct {
	containers []docker.Container
	inspect    map[string]docker.Inspection
	images     map[string]docker.ContainerImage
	existing   map[string]bool
	tags       map[string]string // repo:tag → ID
	untagged   []string
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		containers: []docker.Container{
			{ID: "c1", Names: []string{"/tienda-web-1"}, Labels: map[string]string{"com.docker.compose.service": "web"}},
			{ID: "c2", Names: []string{"/tienda-web-2"}, Labels: map[string]string{"com.docker.compose.service": "web"}},
			{ID: "c3", Names: []string{"/tienda-db-1"}, Labels: map[string]string{"com.docker.compose.service": "db"}},
		},
		inspect: map[string]docker.Inspection{},
		images: map[string]docker.ContainerImage{
			"c1": {ID: imgA, Ref: "tienda-web"},
			"c2": {ID: imgA, Ref: "tienda-web"},
			"c3": {ID: imgB, Ref: "postgres:17"},
		},
		existing: map[string]bool{imgA: true, imgB: true},
		tags:     map[string]string{},
	}
}

func (f *fakeAPI) ProjectContainers(context.Context, string) ([]docker.Container, error) {
	return f.containers, nil
}

func (f *fakeAPI) Inspect(_ context.Context, id string) (docker.Inspection, error) {
	return f.inspect[id], nil
}

func (f *fakeAPI) ImageOf(_ context.Context, id string) (docker.ContainerImage, error) {
	return f.images[id], nil
}

func (f *fakeAPI) ImageExists(_ context.Context, ref string) (bool, error) {
	return f.existing[ref], nil
}

func (f *fakeAPI) Tag(_ context.Context, id, repo, tag string) error {
	f.tags[repo+":"+tag] = id
	return nil
}

func (f *fakeAPI) Untag(_ context.Context, ref string) error {
	f.untagged = append(f.untagged, ref)
	delete(f.tags, ref)
	return nil
}

func (f *fakeAPI) ImagesByReference(context.Context, string) ([]string, error) {
	var refs []string
	for ref := range f.tags {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	return refs, nil
}

func TestNewRejectsSystemdAndRelativeDocker(t *testing.T) {
	p := project()
	p.Runtime, p.ComposeFile, p.ComposeProject, p.Units = proto.RuntimeSystemd, "", "", []string{"x.service"}
	if _, err := New(Config{Docker: "/usr/bin/docker"}, p); err == nil {
		t.Error("New aceptó un proyecto de systemd")
	}
	if _, err := New(Config{Docker: "docker"}, project()); err == nil {
		t.Error("New aceptó docker sin ruta absoluta")
	}
}

func TestCommandIsExplicit(t *testing.T) {
	p := newProject(t)
	c := p.command(0, "up", "--detach", "--no-build")
	want := "compose --ansi never --project-directory /srv/tienda --file /srv/tienda/deploy/compose.yml --project-name tienda up --detach --no-build"
	if got := strings.Join(c.Args, " "); got != want {
		t.Errorf("args =\n%s\nse esperaba\n%s", got, want)
	}
	if c.Path != "/usr/bin/docker" || c.Dir != "/srv/tienda" {
		t.Errorf("comando = %+v", c)
	}
	env := strings.Join(c.Env, "\n")
	for _, want := range []string{"DOCKER_CONFIG=/var/lib/status-sv/docker", "DOCKER_HOST=unix:///run/docker.sock", "HOME=/var/lib/status-sv"} {
		if !strings.Contains(env, want) {
			t.Errorf("falta %s en el entorno:\n%s", want, env)
		}
	}
}

func TestRunRejectsOptionLikeSteps(t *testing.T) {
	p := newProject(t)
	for _, step := range []projectspec.Step{
		{Service: "web", Command: []string{"--entrypoint=sh"}},
		{Service: "-web", Command: []string{"ls"}},
		{Service: "web"},
	} {
		if err := p.Run(context.Background(), step, nil); err == nil || !strings.Contains(err.Error(), "paso inválido") {
			t.Errorf("Run(%+v) = %v", step, err)
		}
	}
}

func TestSnapshotRetainRestore(t *testing.T) {
	p := newProject(t)
	api := newFakeAPI()
	ctx := context.Background()

	images, err := p.Snapshot(ctx, api)
	if err != nil {
		t.Fatal(err)
	}
	// Una vez por servicio, ordenadas.
	if len(images) != 2 || images[0].Service != "db" || images[1] != (ServiceImage{Service: "web", ID: imgA, Ref: "tienda-web"}) {
		t.Fatalf("Snapshot = %+v", images)
	}

	if err := p.Retain(ctx, api, sha1, images); err != nil {
		t.Fatal(err)
	}
	if api.tags["statussv-rollback/tienda/web:0123456789ab"] != imgA || api.tags["statussv-rollback/tienda/db:0123456789ab"] != imgB {
		t.Errorf("etiquetas = %v", api.tags)
	}

	// El build del commit nuevo movió tienda-web a otra imagen; volver atrás
	// la devuelve a la anotada.
	api.tags["tienda-web:latest"] = "sha256:nueva"
	if err := p.Restore(ctx, api, images); err != nil {
		t.Fatal(err)
	}
	if api.tags["tienda-web:latest"] != imgA || api.tags["postgres:17"] != imgB {
		t.Errorf("después de volver: %v", api.tags)
	}
}

func TestRestoreChecksEveryImageFirst(t *testing.T) {
	p := newProject(t)
	api := newFakeAPI()
	delete(api.existing, imgB)
	images := []ServiceImage{{Service: "web", ID: imgA, Ref: "tienda-web"}, {Service: "db", ID: imgB, Ref: "postgres:17"}}

	err := p.Restore(context.Background(), api, images)
	if !errors.Is(err, ErrMissingImage) {
		t.Fatalf("Restore = %v, se esperaba ErrMissingImage", err)
	}
	if len(api.tags) != 0 {
		t.Errorf("tocó etiquetas antes de saber que faltaba una: %v", api.tags)
	}
}

func TestRestoreLeavesDigestReferences(t *testing.T) {
	p := newProject(t)
	api := newFakeAPI()
	images := []ServiceImage{{Service: "cache", ID: imgA, Ref: "redis@sha256:abc"}}
	if err := p.Restore(context.Background(), api, images); err != nil {
		t.Fatal(err)
	}
	if len(api.tags) != 0 {
		t.Errorf("etiquetó una referencia por digest: %v", api.tags)
	}
}

func TestPruneKeepsRecentCommits(t *testing.T) {
	p := newProject(t)
	api := newFakeAPI()
	api.tags = map[string]string{
		"statussv-rollback/tienda/web:0123456789ab": imgA,
		"statussv-rollback/tienda/web:89abcdef0123": imgA,
		"statussv-rollback/tienda/web:ffffffffffff": imgA,
		// Otro proyecto cuyo nombre empieza igual.
		"statussv-rollback/tienda-vieja/web:ffffffffffff": imgA,
	}
	if err := p.Prune(context.Background(), api, []string{sha1, sha2}); err != nil {
		t.Fatal(err)
	}
	if len(api.untagged) != 1 || api.untagged[0] != "statussv-rollback/tienda/web:ffffffffffff" {
		t.Errorf("sacó = %v", api.untagged)
	}
}

func TestRollbackRepo(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"tienda", "web"}:          "statussv-rollback/tienda/web",
		{"Mi_App", "Worker.Queue"}: "statussv-rollback/mi-app/worker-queue",
		{"app", "__raro__"}:        "statussv-rollback/app/raro",
	} {
		if got := RollbackRepo(in[0], in[1]); got != want {
			t.Errorf("RollbackRepo(%q, %q) = %q, se esperaba %q", in[0], in[1], got, want)
		}
	}
}

func TestCheck(t *testing.T) {
	p := newProject(t)
	running := func(health string) docker.Inspection {
		var in docker.Inspection
		in.State.Status = "running"
		if health != "" {
			in.State.Health = &struct {
				Status string `json:"Status"`
			}{Status: health}
		}
		return in
	}
	withStatus := func(status string) docker.Inspection {
		var in docker.Inspection
		in.State.Status = status
		return in
	}
	exited := func(code int) docker.Inspection {
		var in docker.Inspection
		in.State.Status, in.State.ExitCode = "exited", code
		return in
	}

	tests := []struct {
		name    string
		inspect map[string]docker.Inspection
		ready   bool
		detail  string
	}{
		{name: "todo bien", inspect: map[string]docker.Inspection{"c1": running(""), "c2": running("healthy"), "c3": exited(0)}, ready: true, detail: "3 contenedores bien"},
		{name: "healthcheck arrancando", inspect: map[string]docker.Inspection{"c1": running(""), "c2": running("starting"), "c3": running("")}, detail: "tienda-web-2: healthcheck todavía arrancando"},
		{name: "unhealthy", inspect: map[string]docker.Inspection{"c1": running("unhealthy"), "c2": running(""), "c3": running("")}, detail: "tienda-web-1: healthcheck unhealthy"},
		{name: "terminó mal", inspect: map[string]docker.Inspection{"c1": running(""), "c2": running(""), "c3": exited(1)}, detail: "tienda-db-1: terminó con código 1"},
		{name: "reiniciándose", inspect: map[string]docker.Inspection{"c1": withStatus("restarting")}, detail: "tienda-web-1: reiniciándose"},
		{name: "creado sin arrancar", inspect: map[string]docker.Inspection{"c1": running(""), "c2": withStatus("created")}, detail: "tienda-web-2: created"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI()
			api.inspect = tt.inspect
			status, err := p.Check(context.Background(), api)
			if err != nil {
				t.Fatal(err)
			}
			if status.Ready != tt.ready || status.Detail != tt.detail {
				t.Errorf("Check = %+v", status)
			}
		})
	}

	api := newFakeAPI()
	api.containers = nil
	if status, _ := p.Check(context.Background(), api); status.Ready {
		t.Error("un proyecto sin contenedores quedó listo")
	}
}
