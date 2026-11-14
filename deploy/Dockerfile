# Imágenes del control plane y del agent.
#
# El agent en producción va como binario bajo systemd, no en un contenedor: esta
# imagen existe para la prueba de punta a punta, donde hacen falta las dos piezas
# aisladas y descartables.
#
#   docker build -f deploy/Dockerfile --target server -t status-sv-server .
#   docker build -f deploy/Dockerfile --target agent  -t status-sv-agent .

# De dónde salen las imágenes oficiales. Por defecto, Docker Hub; el CI pasa
# mirror.gcr.io/library, la caché de Google, porque los runners se quedan sin
# cupo de Docker Hub.
ARG IMAGENES=docker.io/library

# --- Panel web ---
#
# Node solo aparece acá: el panel se compila a archivos estáticos que el binario
# del control plane se lleva adentro. En el servidor no corre Node.
FROM ${IMAGENES}/node:22-alpine AS web

WORKDIR /web

# Las dependencias primero, por la caché de capas.
COPY web/package.json web/package-lock.json ./
RUN npm ci

COPY web/ ./
# Los mapas de fuentes no viajan adentro del binario: son megabytes que el
# navegador solo pide si alguien abre las herramientas de desarrollo.
RUN npm run build && find dist -name "*.map" -delete

# --- Binarios ---
FROM ${IMAGENES}/golang:1.27-alpine AS builder

WORKDIR /src

# Las dependencias primero: mientras go.mod y go.sum no cambien, esta capa se
# reutiliza y el build es mucho más rápido.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# El panel compilado, en el lugar donde lo busca go:embed. Sin esto el binario
# igual compila, pero la raíz devuelve la página de aviso.
COPY --from=web /web/dist/ ./server/internal/panel/dist/

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
ENV LDBASE=github.com/JuanBaez77/status-sv/internal/buildinfo

RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X ${LDBASE}.Version=${VERSION} -X ${LDBASE}.Commit=${COMMIT} -X ${LDBASE}.Date=${BUILD_DATE}" \
    -o /out/server ./server/cmd/server \
 && CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X ${LDBASE}.Version=${VERSION} -X ${LDBASE}.Commit=${COMMIT} -X ${LDBASE}.Date=${BUILD_DATE}" \
    -o /out/agent ./agent/cmd/agent

# --- Control plane ---
FROM ${IMAGENES}/alpine:3.22 AS server

# ca-certificates para poder hablar HTTPS con lo que haga falta.
RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10001 statussv

COPY --from=builder /out/server /usr/local/bin/server

USER statussv
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
CMD ["serve"]

# --- Agent ---
FROM ${IMAGENES}/alpine:3.22 AS agent

RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10002 statussv \
 && mkdir -p /var/lib/status-sv \
 && chown statussv:statussv /var/lib/status-sv

COPY --from=builder /out/agent /usr/local/bin/agent

USER statussv
ENTRYPOINT ["/usr/local/bin/agent"]
CMD ["run"]

# --- Backup del control plane ---
#
# pg_dump de la misma versión que la base, y restic para cifrar y subir la copia
# (deploy/backup/backup.sh). El script viaja adentro de la imagen: el Docker de
# snap no deja montar archivos de cualquier carpeta del servidor.
FROM ${IMAGENES}/postgres:17-alpine AS backup

RUN apk add --no-cache restic

COPY deploy/backup/backup.sh /usr/local/bin/backup.sh

ENV RESTIC_CACHE_DIR=/cache
ENTRYPOINT ["/usr/local/bin/backup.sh"]
CMD ["servicio"]
