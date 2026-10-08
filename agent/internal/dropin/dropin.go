// Package dropin arma el drop-in 20-deploy.conf, que prende el deploy en un
// servidor (ADR-015): los permisos de más que la unidad del agent necesita
// para desplegar, y un ReadWritePaths por cada workdir declarado.
//
// Lo escriben el instalador (con `statussv-agent deploy-dropin`) y el comando
// que acepta un proyecto nuevo (ADR-022), así los dos dejan exactamente lo
// mismo.
package dropin

import (
	"fmt"
	"strings"

	"github.com/JuanBaez77/status-sv/proto"
)

// DefaultPath es dónde vive el drop-in.
const DefaultPath = "/etc/systemd/system/statussv-agent.service.d/20-deploy.conf"

// hiddenPrefixes son los directorios que ProtectHome=true esconde del agent:
// un workdir ahí no se puede desplegar (ADR-015).
var hiddenPrefixes = []string{"/home/", "/root/", "/run/user/"}

// Hidden dice si un workdir queda escondido para el agent.
func Hidden(workdir string) bool {
	for _, prefix := range hiddenPrefixes {
		if strings.HasPrefix(workdir+"/", prefix) {
			return true
		}
	}
	return false
}

// Render arma el drop-in con los workdir de los proyectos declarados. Devuelve
// además un aviso por cada proyecto que queda afuera.
func Render(state proto.ProjectsState) (string, []string) {
	var b strings.Builder
	var warnings []string
	b.WriteString("# Lo escribe deploy/install-agent.sh --enable-deploy (o statussv-agent project accept); no editar a mano.\n")
	b.WriteString("# El porqué de cada renglón está en docs/adr/ADR-015-motor-de-deploy-docker.md.\n")
	b.WriteString("[Service]\n")
	b.WriteString("Environment=STATUSSV_DEPLOY_ENABLED=true\n")
	b.WriteString("UMask=0002\n")
	b.WriteString("MemoryMax=512M\n")
	b.WriteString("TasksMax=256\n")
	for _, p := range state.Projects {
		if Hidden(p.Workdir) {
			warnings = append(warnings, fmt.Sprintf("%s está en %s, que la unidad esconde (ProtectHome); no se va a poder desplegar", p.Name, p.Workdir))
			continue
		}
		// El - hace que systemd arranque igual si el directorio todavía no
		// existe.
		fmt.Fprintf(&b, "ReadWritePaths=-%s\n", p.Workdir)
	}
	return b.String(), warnings
}
