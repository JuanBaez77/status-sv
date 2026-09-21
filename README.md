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
│   └── internal/     # config, state, selfstat, ws; después collector, docker, systemd, deploy
├── server/           # Go: API + MCP + WebSocket + embed del front
│   ├── cmd/server/
│   ├── internal/     # api, auth, config, enrollment, store; después mcp, metrics, approvals
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
GOOS=linux golangci-lint run   # como lo corre el CI: hay archivos que solo existen en Linux
```

`go test -race` necesita un compilador de C, así que en Windows conviene dejar el detector de
carreras para el CI y correr `go test ./...` a secas.

## Base de datos de desarrollo

```bash
cp .env.example .env    # ajustar la contraseña
make db-up              # PostgreSQL 17 en 127.0.0.1:5433
make db-down            # apaga y conserva los datos
make db-reset           # apaga y borra los datos
```

El puerto por defecto es **5433**, no 5432: así no choca con otros PostgreSQL que ya estén
corriendo en la máquina, y en Windows evita el rango de puertos que reserva el sistema. Se puede
cambiar con `POSTGRES_PORT` en el `.env`. La base solo escucha en `127.0.0.1`.

Las herramientas que generan y migran el esquema se instalan una vez:

```bash
go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
go install github.com/pressly/goose/v3/cmd/goose@latest
```

Compilar sqlc desde fuente tarda varios minutos. La alternativa, sin instalar nada, es la imagen
oficial:

```bash
docker run --rm -v "$PWD:/src" -w /src sqlc/sqlc:1.31.1 generate
```

El esquema vive en `server/migrations/`, versionado con goose y embebido en el binario del
servidor. El código de acceso a datos se genera con sqlc a partir de `server/internal/store/queries/`:

```bash
make sqlc          # regenera el código
make sqlc-check    # falla si el código generado quedó desactualizado
```

Los tests de integración necesitan una base de verdad y se saltean si no encuentran
`TEST_DATABASE_URL`:

```bash
TEST_DATABASE_URL='postgres://statussv:...@127.0.0.1:5433/statussv_test?sslmode=disable' make test-integration
```

Cada paquete de tests **crea su propia base** a partir de esa cadena, le aplica las migraciones y
la borra al terminar. Es lo que permite que los paquetes corran en paralelo sin pisarse: cuando
compartían base, el `TRUNCATE` de uno borraba los datos del otro. Por eso el usuario de
`TEST_DATABASE_URL` necesita permiso para crear bases.

### Prueba de punta a punta

Levanta PostgreSQL, el control plane y un agent en contenedores, enrola el agent de verdad y
verifica lo que define el criterio de terminado del Sprint 0: que el latido llegue a la base y que
sobreviva a un reinicio del control plane.

```bash
./deploy/e2e/run.sh
```

Se limpia sola al terminar, incluso si falla, y en ese caso imprime los registros del control
plane y del agent. Es el mismo script que corre el CI.

## Control plane

```bash
go run ./server/cmd/server help              # todos los subcomandos
go run ./server/cmd/server migrate up        # aplica las migraciones
go run ./server/cmd/server migrate status    # qué está aplicado y qué no
go run ./server/cmd/server enroll-token --server vps-01
go run ./server/cmd/server serve             # levanta la API
```

Las migraciones **no** se aplican solas al arrancar: se corren a propósito con `migrate up`, para
poder revisarlas antes de tocar producción.

El token de enrolamiento se imprime **una sola vez**: la base guarda nada más que su hash. Si se
pierde, se genera otro.

Endpoints de agents:

| Ruta | Qué hace |
| --- | --- |
| `POST /v1/agents/enroll` | Canjea un token de un solo uso por la credencial propia del agent. Límite de frecuencia por IP. |
| `GET /v1/agents/connect` | Canal permanente por WebSocket, autenticado con `Authorization: Bearer <credencial>`. |

Los dos hablan el mismo formato que define [`proto/`](proto/): un envelope con versión. Si se
enrola un servidor que ya existía (una reinstalación, por ejemplo), se reutiliza el registro y se
revoca la credencial anterior, así no quedan duplicados ni dos credenciales válidas.

Chequeos de salud:

| Ruta | Qué dice | Para qué |
| --- | --- | --- |
| `GET /healthz` | El proceso está vivo. No toca la base. | Lo mira el supervisor para decidir si reiniciar |
| `GET /readyz` | La base responde. Devuelve 503 si no. | Lo mira el balanceador antes de mandar tráfico |

## Agent

El agent se enrola una vez y después mantiene el canal abierto. El token viaja por variable de
entorno y no por argumento, porque los argumentos de un proceso los puede leer cualquier usuario
del sistema con `ps`:

```bash
export STATUSSV_SERVER_URL=https://status.example.com
export STATUSSV_ENROLL_TOKEN=svenr_...
go run ./agent/cmd/agent enroll
go run ./agent/cmd/agent run
```

La credencial queda en `/var/lib/status-sv/agent.json` con permisos `0600`, y es el único lugar
donde existe: el control plane guarda solo su hash. Si se pierde el archivo, hay que enrolar de
nuevo con un token nuevo.

El agent **nunca se rinde**: si el control plane se cae, si la red se corta o si la credencial fue
revocada, espera con backoff exponencial (de 1 s a 1 minuto, con dispersión) y vuelve a intentar.
Cada lectura tiene plazo, así que un canal que quedó mudo sin cerrarse también termina en una
reconexión.

El agent es **solo Linux** en producción: la memoria y el CPU propios se leen de `/proc`. En
Windows y macOS compila y corre para desarrollar, con una medición aproximada.

## Documentación

- [docs/runbook-despliegue.md](docs/runbook-despliegue.md) — cómo desplegar el control plane y el agent en un VPS.
- [docs/DISENO.md](docs/DISENO.md) — diseño, modelo de datos, protocolo, seguridad y roadmap.
- [docs/adr/](docs/adr/) — decisiones de arquitectura, una por archivo.
- [docs/mas-adelante.md](docs/mas-adelante.md) — ideas fuera del alcance actual.

## Convenciones

- Ramas cortas desde `main`, un PR por unidad de trabajo y CI verde para mergear.
- Commits con [Conventional Commits](https://www.conventionalcommits.org/).
- Código e identificadores en inglés; documentación, ADR y commits en español.
- Nunca se commitean secretos: van en variables de entorno, con `.env.example` como referencia.
