#!/usr/bin/env bash
#
# Prueba de punta a punta del Sprint 0: levanta PostgreSQL, el control plane y un
# agent en contenedores, y verifica lo que define el criterio de terminado:
#
#   1. El agent se enrola solo con un token de un solo uso.
#   2. El latido actualiza servers.last_seen.
#   3. Si el control plane se cae, el agent reconecta y el latido se reanuda.
#
# Se corre igual en local y en CI:
#
#   ./deploy/e2e/run.sh
#
set -euo pipefail

cd "$(dirname "$0")/../.."

COMPOSE=(docker compose -f deploy/docker-compose.e2e.yml)

# El compose exige el token para el agent, y docker compose interpola el archivo
# entero en cada comando: sin esto, hasta un simple `exec` fallaría antes de que
# el token exista. Se reemplaza por el de verdad al levantar el agent.
export STATUSSV_ENROLL_TOKEN="${STATUSSV_ENROLL_TOKEN:-todavia-no-hay-token}"
SERVER_NAME="vps-e2e"

# Cuánto se espera cada condición antes de darla por fallada.
TIMEOUT_SECONDS=120

rojo() { printf '\033[31m%s\033[0m\n' "$*" >&2; }
verde() { printf '\033[32m%s\033[0m\n' "$*"; }
paso() { printf '\n=== %s ===\n' "$*"; }

limpiar() {
  local salida=$?
  if [ $salida -ne 0 ]; then
    rojo "la prueba falló; últimos registros:"
    "${COMPOSE[@]}" logs --tail 40 server agent 2>&1 || true
  fi
  paso "limpiando"
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  exit $salida
}
trap limpiar EXIT

# psql corre una consulta contra la base de la prueba.
psql_query() {
  "${COMPOSE[@]}" exec -T postgres psql -U statussv -d statussv -tAc "$1"
}

# esperar repite una condición hasta que se cumpla o se acabe el tiempo.
esperar() {
  local descripcion="$1"
  shift

  local fin=$((SECONDS + TIMEOUT_SECONDS))
  until "$@" >/dev/null 2>&1; do
    if [ $SECONDS -ge $fin ]; then
      rojo "se agotó el tiempo esperando: ${descripcion}"
      return 1
    fi
    sleep 1
  done
  verde "ok: ${descripcion}"
}

# --- 1. Base y control plane ---

paso "levantando PostgreSQL y el control plane"
"${COMPOSE[@]}" up -d --build postgres server

esperar "el control plane responde /readyz" \
  "${COMPOSE[@]}" exec -T server wget -q -O- http://127.0.0.1:8080/readyz

paso "aplicando las migraciones"
"${COMPOSE[@]}" exec -T server /usr/local/bin/server migrate up

# --- 2. Token de enrolamiento ---

paso "generando el token de enrolamiento"
TOKEN="$(
  "${COMPOSE[@]}" exec -T server /usr/local/bin/server enroll-token --server "${SERVER_NAME}" \
    | grep -o 'svenr_[A-Za-z0-9_-]*' | head -1
)"

if [ -z "${TOKEN}" ]; then
  rojo "el control plane no devolvió ningún token"
  exit 1
fi
verde "ok: token generado (${TOKEN:0:12}...)"

# --- 3. El agent se enrola y late ---

paso "levantando el agent"
export STATUSSV_ENROLL_TOKEN="${TOKEN}"
"${COMPOSE[@]}" up -d --build agent

esperar "el servidor queda registrado" \
  bash -c "[ \"\$(${COMPOSE[*]} exec -T postgres psql -U statussv -d statussv -tAc \"SELECT count(*) FROM servers WHERE name = '${SERVER_NAME}'\" | tr -d '[:space:]')\" = '1' ]"

esperar "el token quedó consumido" \
  bash -c "[ \"\$(${COMPOSE[*]} exec -T postgres psql -U statussv -d statussv -tAc 'SELECT count(*) FROM enrollment_tokens WHERE used_at IS NOT NULL' | tr -d '[:space:]')\" = '1' ]"

esperar "hay una credencial activa" \
  bash -c "[ \"\$(${COMPOSE[*]} exec -T postgres psql -U statussv -d statussv -tAc 'SELECT count(*) FROM agent_credentials WHERE revoked_at IS NULL' | tr -d '[:space:]')\" = '1' ]"

esperar "el latido actualizó last_seen" \
  bash -c "[ \"\$(${COMPOSE[*]} exec -T postgres psql -U statussv -d statussv -tAc \"SELECT count(*) FROM servers WHERE name = '${SERVER_NAME}' AND last_seen_at IS NOT NULL\" | tr -d '[:space:]')\" = '1' ]"

# El latido no es un evento único: tiene que seguir llegando.
paso "verificando que el latido se repite"
PRIMER_LATIDO="$(psql_query "SELECT last_seen_at FROM servers WHERE name = '${SERVER_NAME}'")"

esperar "llegó un latido posterior" \
  bash -c "[ \"\$(${COMPOSE[*]} exec -T postgres psql -U statussv -d statussv -tAc \"SELECT count(*) FROM servers WHERE name = '${SERVER_NAME}' AND last_seen_at > '${PRIMER_LATIDO}'\" | tr -d '[:space:]')\" = '1' ]"

# --- 4. Reconexión ---

paso "reiniciando el control plane para forzar una reconexión"
ANTES_DEL_CORTE="$(psql_query "SELECT last_seen_at FROM servers WHERE name = '${SERVER_NAME}'")"

"${COMPOSE[@]}" restart server >/dev/null

esperar "el control plane volvió" \
  "${COMPOSE[@]}" exec -T server wget -q -O- http://127.0.0.1:8080/readyz

esperar "el agent reconectó solo y el latido se reanudó" \
  bash -c "[ \"\$(${COMPOSE[*]} exec -T postgres psql -U statussv -d statussv -tAc \"SELECT count(*) FROM servers WHERE name = '${SERVER_NAME}' AND last_seen_at > '${ANTES_DEL_CORTE}'\" | tr -d '[:space:]')\" = '1' ]"

# El agent no se reenroló: sigue habiendo un solo servidor y una sola credencial.
CREDENCIALES="$(psql_query "SELECT count(*) FROM agent_credentials" | tr -d '[:space:]')"
if [ "${CREDENCIALES}" != "1" ]; then
  rojo "después de reconectar hay ${CREDENCIALES} credenciales; se esperaba 1"
  exit 1
fi
verde "ok: la reconexión reutilizó la credencial existente"

paso "resumen"
psql_query "SELECT name || ' | agent ' || agent_version || ' | último latido hace ' || round(extract(epoch from now() - last_seen_at)) || 's' FROM servers"

verde "la prueba de punta a punta pasó"
