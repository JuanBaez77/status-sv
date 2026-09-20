# status-sv

Plataforma propia para **observar y operar servidores VPS**: un panel con métricas y estado en vivo,
y un MCP remoto para que un LLM despliegue, reconstruya y verifique proyectos sin recibir acceso de shell.

> Estado: **Sprint 0 (fundaciones)**. Todavía no hay binarios funcionales.
> El diseño completo y el roadmap están en [docs/DISENO.md](docs/DISENO.md).

## Qué hace

- Un **agent** por VPS recolecta CPU, RAM, disco, red, contenedores y servicios, y los manda al control plane.
- El **control plane** concentra métricas, API, WebSocket del panel, auditoría y aprobaciones.
- El **MCP** expone operaciones tipadas (`deploy`, `rollback`, `get_logs`) para que un LLM opere sin shell libre.
- El **panel** muestra todos los servidores y proyectos en vivo.

Tres reglas que no se negocian: el agent solo hace conexiones salientes, nunca existe ejecución de shell
arbitraria, y toda escritura sobre producción pasa por una aprobación humana auditada.

## Estructura del repositorio

```text
status-sv/
├── agent/            # Go: binario que corre en cada VPS
│   ├── cmd/agent/
│   └── internal/     # collector, docker, systemd, deploy, ws
├── server/           # Go: API + MCP + WebSocket + embed del front
│   ├── cmd/server/
│   ├── internal/     # api, mcp, auth, store, metrics, approvals
│   └── migrations/   # SQL versionado
├── internal/         # Go: código compartido por agent y server (buildinfo)
├── web/              # React + Vite + TypeScript
├── proto/            # contratos agent <-> server
├── worker-smoke/     # Playwright, a demanda
├── deploy/           # compose y Caddyfile del control plane
├── docs/             # diseño y decisiones (ADR)
└── .github/workflows # CI: lint, tests, build, metas de rendimiento
```

## Requisitos de desarrollo

| Herramienta | Versión | Para qué |
| --- | --- | --- |
| Go | 1.27 o superior | agent y control plane |
| Docker + Compose | v2 | PostgreSQL de desarrollo y tests de integración |
| Node.js | 22 o superior | panel (a partir del Sprint 1) |

El agent es **solo Linux**: lee `/proc` y el socket de Docker. En Windows y macOS se compila y se
corren los tests unitarios, pero se ejecuta en un contenedor o en un VPS.

## Comandos

Con `make` (Linux, macOS o CI):

| Comando | Qué hace |
| --- | --- |
| `make help` | Lista los targets disponibles |
| `make test` | Tests unitarios |
| `make test-race` | Tests con detector de carreras y orden aleatorio |
| `make lint` | `golangci-lint` |
| `make build` | Compila agent y server en `bin/` |
| `make build-linux` | Compila para `linux/amd64` en `bin/linux-amd64/` |
| `make size-check` | Verifica la meta de 15 MB del binario del agent |
| `make ci` | Todo lo que corre el CI |

En Windows no hay `make`; los equivalentes directos son:

```bash
gofmt -l .
go vet ./...
go test ./...
go build -o bin/ ./agent/cmd/agent ./server/cmd/server
golangci-lint run
```

`go test -race` necesita un compilador de C, así que en Windows conviene dejar el detector de
carreras para el CI y correr `go test ./...` a secas.

## Documentación

- [docs/DISENO.md](docs/DISENO.md) — diseño, modelo de datos, protocolo, seguridad y roadmap.
- [docs/adr/](docs/adr/) — decisiones de arquitectura, una por archivo.
- [docs/mas-adelante.md](docs/mas-adelante.md) — ideas fuera del alcance actual.

## Convenciones

- Ramas cortas desde `main`, un PR por unidad de trabajo y CI verde para mergear.
- Commits con [Conventional Commits](https://www.conventionalcommits.org/).
- Código e identificadores en inglés; documentación, ADR y commits en español.
- Nunca se commitean secretos: van en variables de entorno, con `.env.example` como referencia.
