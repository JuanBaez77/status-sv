// Command agent corre como servicio en cada VPS: recolecta métricas y estado, y
// ejecuta las operaciones que le manda el control plane de status-sv.
//
// Todavía es un esqueleto: solo responde --version. El enrolamiento y el latido
// llegan en el PR 6 del Sprint 0.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/JuanBaez77/status-sv/internal/buildinfo"
)

const name = "status-sv-agent"

func main() {
	version := flag.Bool("version", false, "imprime la versión y termina")
	flag.Parse()

	if *version {
		fmt.Println(buildinfo.Line(name))
		return
	}

	fmt.Fprintf(os.Stderr, "%s: todavía sin implementar; el enrolamiento y el latido llegan en el PR 6 del Sprint 0\n", name)
	os.Exit(1)
}
