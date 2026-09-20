# Imágenes del control plane y del agent.
#
# El agent en producción va como binario bajo systemd, no en un contenedor: esta
# imagen existe para la prueba de punta a punta, donde hacen falta las dos piezas
# aisladas y descartables.
#
#   docker build -f deploy/Dockerfile --target server -t status-sv-server .
#   docker build -f deploy/Dockerfile --target agent  -t status-sv-agent .

FROM golang:1.27-alpine AS builder

WORKDIR /src

# Las dependencias primero: mientras go.mod y go.sum no cambien, esta capa se
# reutiliza y el build es mucho más rápido.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

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
FROM alpine:3.22 AS server

# ca-certificates para poder hablar HTTPS con lo que haga falta.
RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10001 statussv

COPY --from=builder /out/server /usr/local/bin/server

USER statussv
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
CMD ["serve"]

# --- Agent ---
FROM alpine:3.22 AS agent

RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10002 statussv \
 && mkdir -p /var/lib/status-sv \
 && chown statussv:statussv /var/lib/status-sv

COPY --from=builder /out/agent /usr/local/bin/agent

USER statussv
ENTRYPOINT ["/usr/local/bin/agent"]
CMD ["run"]
