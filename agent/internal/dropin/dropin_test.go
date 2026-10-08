package dropin

import (
	"strings"
	"testing"

	"github.com/JuanBaez77/status-sv/proto"
)

func TestHidden(t *testing.T) {
	for dir, want := range map[string]bool{
		"/srv/x": false, "/opt/app": false, "/home": true, "/home/juan/x": true, "/root": true, "/run/user/1000/x": true, "/homes/x": false,
	} {
		if Hidden(dir) != want {
			t.Errorf("Hidden(%q) = %v", dir, !want)
		}
	}
}

// El drop-in es el mismo que escribía el instalador: los permisos del deploy
// y un ReadWritePaths por workdir, salvo los que la unidad esconde.
func TestRender(t *testing.T) {
	state := proto.ProjectsState{Projects: []proto.Project{
		{Name: "tienda", Workdir: "/srv/tienda"},
		{Name: "privado", Workdir: "/home/juan/privado"},
		{Name: "svdemo", Workdir: "/srv/svdemo"},
	}}
	content, warnings := Render(state)
	want := `# Lo escribe deploy/install-agent.sh --enable-deploy (o statussv-agent project accept); no editar a mano.
# El porqué de cada renglón está en docs/adr/ADR-015-motor-de-deploy-docker.md.
[Service]
Environment=STATUSSV_DEPLOY_ENABLED=true
UMask=0002
MemoryMax=512M
TasksMax=256
ReadWritePaths=-/srv/tienda
ReadWritePaths=-/srv/svdemo
`
	if content != want {
		t.Errorf("drop-in:\n%s\nse esperaba:\n%s", content, want)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "privado") {
		t.Errorf("avisos = %q", warnings)
	}

	// Sin proyectos, el deploy queda prendido sin ningún workdir.
	if content, _ := Render(proto.ProjectsState{}); strings.Contains(content, "ReadWritePaths") {
		t.Errorf("sin proyectos:\n%s", content)
	}
}
