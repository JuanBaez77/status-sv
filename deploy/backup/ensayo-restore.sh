#!/usr/bin/env bash
#
# Ensayo de restore: prueba que una copia del backup sirve para levantar el
# control plane, sin tocar el que está corriendo.
#
#   ensayo-restore.sh DIR
#
# DIR es una copia ya bajada con "backup.sh restaurar" (ver el runbook): adentro
# tienen que estar work/statussv.dump, run/secrets/totp_key y backup/env/.env.
#
# Levanta una PostgreSQL vacía y descartable, restaura el volcado, corre
# "migrate status" y "migrate up", arranca el control plane con la clave TOTP
# de la copia y espera a que /readyz conteste. Al final muestra cuántas filas
# tienen las tablas principales y lo borra todo.
#
# Variables:
#   STATUSSV_ENSAYO_IMAGEN   la imagen del control plane (obligatoria; en el
#                            servidor, status-sv-server:<versión que corre>)
#   ENSAYO_PROYECTO          el nombre del proyecto de Compose (status-sv-ensayo)
#   ENSAYO_DEJAR=1           no lo borra al terminar, para mirarlo (el e2e lo usa)
set -euo pipefail

DIR="${1:?uso: ensayo-restore.sh DIR (la copia bajada con backup.sh restaurar)}"
DIR="$(cd "${DIR}" && pwd)"
: "${STATUSSV_ENSAYO_IMAGEN:?falta STATUSSV_ENSAYO_IMAGEN: la imagen del control plane, p. ej. status-sv-server:1696eb8}"
PROYECTO="${ENSAYO_PROYECTO:-status-sv-ensayo}"

DUMP="${DIR}/work/statussv.dump"
export STATUSSV_ENSAYO_TOTP_KEY="${DIR}/run/secrets/totp_key"
export STATUSSV_ENSAYO_IMAGEN
COMPOSE=(docker compose -p "${PROYECTO}" -f "$(dirname "$0")/docker-compose.ensayo.yml")

rojo() { printf '\033[31m%s\033[0m\n' "$*" >&2; }
verde() { printf '\033[32m%s\033[0m\n' "$*"; }
paso() { printf '\n=== %s ===\n' "$*"; }

for archivo in "${DUMP}" "${STATUSSV_ENSAYO_TOTP_KEY}" "${DIR}/backup/env/.env"; do
  [ -f "${archivo}" ] || { rojo "falta ${archivo}: ¿es una copia bajada con backup.sh restaurar?"; exit 1; }
done

limpiar() {
  local salida=$?
  if [ $salida -ne 0 ]; then
    rojo "el ensayo falló; últimos registros:"
    "${COMPOSE[@]}" logs --tail 30 2>&1 || true
  fi
  if [ "${ENSAYO_DEJAR:-0}" != 1 ]; then
    "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  fi
  exit $salida
}
trap limpiar EXIT

psql_ensayo() {
  "${COMPOSE[@]}" exec -T postgres psql -U statussv -d statussv -v ON_ERROR_STOP=1 -tAc "$1"
}

paso "PostgreSQL vacía"
"${COMPOSE[@]}" up -d --wait postgres
verde "ok: la base del ensayo está lista"

paso "Restaurar el volcado"
"${COMPOSE[@]}" cp "${DUMP}" postgres:/tmp/statussv.dump
# --no-owner: los objetos quedan del usuario que restaura, que acá es el mismo.
"${COMPOSE[@]}" exec -T postgres pg_restore -U statussv -d statussv \
  --no-owner --exit-on-error /tmp/statussv.dump
verde "ok: pg_restore sin errores"

paso "Migraciones"
"${COMPOSE[@]}" run --rm --no-deps server migrate status
"${COMPOSE[@]}" run --rm --no-deps server migrate up
verde "ok: migrate status y migrate up"

paso "El control plane sobre lo restaurado"
"${COMPOSE[@]}" up -d --wait server
"${COMPOSE[@]}" exec -T server wget -q -O- http://127.0.0.1:8080/readyz
echo
verde "ok: /readyz contesta con la base y la clave TOTP de la copia"

paso "Lo que trajo la copia"
for tabla in users servers projects deploys api_tokens audit_log; do
  printf '%-12s %s filas\n' "${tabla}" "$(psql_ensayo "SELECT count(*) FROM ${tabla}")"
done

verde "ensayo de restore: OK"
