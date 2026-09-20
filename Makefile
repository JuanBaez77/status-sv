# Makefile de status-sv.
# En Windows no hay make: el README tiene los comandos equivalentes con go.

GO      ?= go
BIN     ?= bin
PKG     := github.com/JuanBaez77/status-sv
LDBASE  := $(PKG)/internal/buildinfo

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w -X $(LDBASE).Version=$(VERSION) -X $(LDBASE).Commit=$(COMMIT) -X $(LDBASE).Date=$(DATE)

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
	@archivos=$$(gofmt -l .); \
	if [ -n "$$archivos" ]; then echo "Archivos sin gofmt:"; echo "$$archivos"; exit 1; fi

.PHONY: vet
vet: ## Análisis estático con go vet
	$(GO) vet ./...

.PHONY: lint
lint: ## Corre golangci-lint
	golangci-lint run

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
	@tamano=$$(wc -c < $(BIN)/linux-amd64/agent); \
	echo "agent: $$tamano bytes (máximo $(AGENT_MAX_BYTES))"; \
	if [ "$$tamano" -gt "$(AGENT_MAX_BYTES)" ]; then echo "FALLA: el binario del agent supera la meta de 15 MB"; exit 1; fi

.PHONY: tidy
tidy: ## Ordena go.mod y go.sum
	$(GO) mod tidy

.PHONY: ci
ci: fmt-check vet lint test-race size-check ## Todo lo que corre el CI

.PHONY: clean
clean: ## Borra los binarios compilados
	rm -rf $(BIN)
