# Makefile de status-sv.
# En Windows no hay make: el README tiene los comandos equivalentes con go.

GO      ?= go
# sqlc y goose se instalan aparte; ver la sección de base de datos del README.
SQLC    ?= sqlc
SQLC_VERSION ?= 1.31.1
BIN     ?= bin
PKG     := github.com/JuanBaez77/status-sv
LDBASE  := $(PKG)/internal/buildinfo

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w -X $(LDBASE).Version=$(VERSION) -X $(LDBASE).Commit=$(COMMIT) -X $(LDBASE).Date=$(DATE)

# El panel compilado se copia acá antes de construir el binario: de ahí lo toma
# go:embed (ver server/internal/panel).
PANEL_DIST := server/internal/panel/dist
# Deja dist/ con lo único que se versiona: el aviso y su .gitignore.
PANEL_CLEAN := find $(PANEL_DIST) -mindepth 1 ! -name .gitignore ! -name sin-panel.html -delete

# Meta de tamaño del binario del agent: 15 MB (ver docs/DISENO.md).
AGENT_MAX_BYTES := 15728640

.DEFAULT_GOAL := help

.PHONY: help
help: ## Muestra esta ayuda
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

.PHONY: fmt
fmt: ## Formatea el código Go
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check: ## Falla si hay archivos Go sin formatear
	@files=$$(gofmt -l .); \
	if [ -n "$$files" ]; then echo "Archivos sin gofmt:"; echo "$$files"; exit 1; fi

.PHONY: vet
vet: ## Análisis estático con go vet
	$(GO) vet ./...

.PHONY: lint
lint: ## Corre golangci-lint para este sistema
	golangci-lint run

.PHONY: lint-linux
lint-linux: ## Corre golangci-lint como lo hace el CI, para linux
	GOOS=linux golangci-lint run

.PHONY: test
test: ## Tests unitarios
	$(GO) test ./...

.PHONY: test-race
test-race: ## Tests unitarios con detector de carreras y orden aleatorio
	$(GO) test -race -shuffle=on ./...

.PHONY: build
build: ## Compila agent y server para este sistema, en bin/
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/ ./agent/cmd/agent ./server/cmd/server

.PHONY: build-linux
build-linux: ## Compila para linux/amd64, en bin/linux-amd64/
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/linux-amd64/ ./agent/cmd/agent ./server/cmd/server

.PHONY: size-check
size-check: build-linux ## Verifica la meta de 15 MB del binario del agent
	@size=$$(wc -c < $(BIN)/linux-amd64/agent); \
	echo "agent: $$size bytes (máximo $(AGENT_MAX_BYTES))"; \
	if [ "$$size" -gt "$(AGENT_MAX_BYTES)" ]; then echo "FALLA: el binario del agent supera la meta de 15 MB"; exit 1; fi

.PHONY: tidy
tidy: ## Ordena go.mod y go.sum
	$(GO) mod tidy

.PHONY: db-up
db-up: ## Levanta el PostgreSQL de desarrollo
	docker compose -f deploy/docker-compose.dev.yml up -d

.PHONY: db-down
db-down: ## Apaga el PostgreSQL de desarrollo y conserva los datos
	docker compose -f deploy/docker-compose.dev.yml down

.PHONY: db-reset
db-reset: ## Apaga el PostgreSQL de desarrollo y borra los datos
	docker compose -f deploy/docker-compose.dev.yml down -v

.PHONY: sqlc
sqlc: ## Regenera el código de acceso a datos a partir de las queries
	$(SQLC) generate

.PHONY: sqlc-check
sqlc-check: ## Falla si el código generado no coincide con las queries
	$(SQLC) diff

.PHONY: test-e2e
test-e2e: ## Prueba de punta a punta con Docker: control plane, base y agent
	./deploy/e2e/run.sh

.PHONY: test-e2e-deploy
test-e2e-deploy: ## Deploy de punta a punta con el Docker de la máquina (necesita DATABASE_URL)
	./deploy/e2e/deploy.sh

.PHONY: test-carga
test-carga: ## Prueba de carga: 10 agents simulados y las metas de rendimiento
	./deploy/carga/run.sh

.PHONY: test-integration
test-integration: ## Tests de integración; necesita TEST_DATABASE_URL
	$(GO) test -tags=integration -count=1 ./...

.PHONY: ci
ci: fmt-check vet lint lint-linux sqlc-check test-race size-check web-ci ## Todo lo que corre el CI, menos los tests de integración

.PHONY: web-install
web-install: ## Instala las dependencias del panel
	cd web && npm ci

.PHONY: web-ci
web-ci: ## Lint, tipos, tests, build y meta de tamaño del panel
	cd web && npm run ci

.PHONY: web-embed
web-embed: ## Compila el panel y lo deja adentro del paquete que lo embebe
	cd web && npm run build
	$(PANEL_CLEAN)
	cp -R web/dist/. $(PANEL_DIST)/
	# Los mapas de fuentes no viajan adentro del binario: son megabytes que el
	# navegador solo pide si alguien abre las herramientas de desarrollo.
	find $(PANEL_DIST) -name "*.map" -delete

.PHONY: build-release
build-release: web-embed build-linux ## Panel adentro y binarios para linux, como en producción

.PHONY: clean
clean: ## Borra los binarios compilados y el panel embebido
	rm -rf $(BIN)
	$(PANEL_CLEAN)
