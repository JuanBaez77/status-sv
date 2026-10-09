#!/usr/bin/env bash
# El Docker del runner baja las imágenes de Docker Hub por mirror.gcr.io.
#
# Los runners de GitHub comparten IPs, y Docker Hub sin cuenta da 100
# descargas cada seis horas por IP: cuando se acaban, cualquier job que baje o
# compile una imagen falla con 429 "toomanyrequests" (pasó el 2026-10-09 en
# varios jobs seguidos, también en main). mirror.gcr.io es la caché de Google
# de las imágenes de Docker Hub, sin ese límite.
#
# Solo cambia el Docker del runner, y con un reload: registry-mirrors es de lo
# que dockerd relee sin reiniciarse, así que los contenedores de servicio del
# job (el PostgreSQL) siguen andando. Si el espejo no tiene una imagen, Docker
# la busca en Docker Hub como siempre. Producción no se entera.
set -euo pipefail

readonly CONFIG=/etc/docker/daemon.json
readonly ESPEJO=https://mirror.gcr.io

actual='{}'
if sudo test -s "${CONFIG}"; then
  actual="$(sudo cat "${CONFIG}")"
fi

nuevo="$(jq --arg espejo "${ESPEJO}" '.["registry-mirrors"] = [$espejo]' <<<"${actual}")"
sudo tee "${CONFIG}" >/dev/null <<<"${nuevo}"
sudo systemctl reload docker

# reload vuelve antes de que dockerd termine de releer: se espera a verlo.
for _ in $(seq 1 20); do
  if docker info --format '{{json .RegistryConfig.Mirrors}}' | grep -q "mirror.gcr.io"; then
    echo "Docker Hub por ${ESPEJO}"
    exit 0
  fi
  sleep 0.5
done
echo "dockerd no tomó el espejo; sigue con Docker Hub" >&2
docker info --format '{{json .RegistryConfig}}' >&2
exit 1
