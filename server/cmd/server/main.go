// Command server es el control plane de status-sv: API REST, WebSocket del panel,
// servidor MCP y agregación de métricas.
//
// Todavía es un esqueleto: solo responde --version. Los endpoints de salud, el
// enrolamiento y el WebSocket de agents llegan en el PR 5 del Sprint 0.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/JuanBaez77/status-sv/internal/buildinfo"
)

const name = "status-sv-server"

func main() {
	version := flag.Bool("version", false, "imprime la versión y termina")
	flag.Parse()

	if *version {
		fmt.Println(buildinfo.Line(name))
		return
	}

	fmt.Fprintf(os.Stderr, "%s: todavía sin implementar; la API y el WebSocket de agents llegan en el PR 5 del Sprint 0\n", name)
	os.Exit(1)
}
