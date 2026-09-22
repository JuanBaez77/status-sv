#!/usr/bin/env bash
#
# Prueba de punta a punta: levanta PostgreSQL, el control plane y un agent en
# contenedores, y verifica el camino completo:
#
#   1. El agent se enrola solo con un token de un solo uso.
#   2. El latido actualiza servers.last_seen.
#   3. Las métricas del agent llegan a la base, con sus discos e interfaces.
#   4. Si el control plane se cae, el agent reconecta, y el latido y las
#      métricas se reanudan.
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

# --- Usuario del panel ---

# Sin terminal, user-add lee la contraseña de una línea de la entrada. Es solo
# para esta prueba: en el VPS se escribe en la terminal, sin eco.
paso "creando un usuario del panel e iniciando sesión"
E2E_PASSWORD="contraseña-de-la-prueba-e2e"
printf '%s\n' "${E2E_PASSWORD}" | "${COMPOSE[@]}" exec -T server /usr/local/bin/server user-add --username e2e

# wget de busybox con -S deja los encabezados de la respuesta en stderr.
SESION="$(
  "${COMPOSE[@]}" exec -T server wget -S -q -O /dev/null \
    --header 'Content-Type: application/json' \
    --post-data "{\"username\":\"e2e\",\"password\":\"${E2E_PASSWORD}\"}" \
    http://127.0.0.1:8080/v1/auth/login 2>&1 \
    | grep -io '__Host-statussv_session=svses_[A-Za-z0-9_-]*' | head -1
)"
if [ -z "${SESION}" ]; then
  rojo "el login no devolvió la cookie de sesión"
  exit 1
fi
verde "ok: el login entregó la cookie de sesión"

YO="$("${COMPOSE[@]}" exec -T server wget -q -O- --header "Cookie: ${SESION}" http://127.0.0.1:8080/v1/auth/me)"
case "${YO}" in
  *'"username":"e2e"'*) verde "ok: la sesión identifica al usuario" ;;
  *) rojo "/v1/auth/me respondió: ${YO}"; exit 1 ;;
esac

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

# --- 4. Métricas ---

# La primera muestra sale a los 10 s de arrancar el agent, que es lo que
# tarda en tener dos lecturas de CPU para comparar.
paso "verificando que las métricas llegan a la base"

# metricas_de TABLA cuenta las filas del agent en una tabla de métricas.
metricas_de() {
  psql_query "SELECT count(*) FROM $1 m JOIN servers s ON s.id = m.server_id WHERE s.name = '${SERVER_NAME}'" | tr -d '[:space:]'
}
hay_metricas() {
  [ "$(metricas_de "$1")" -gt "${2:-0}" ]
}

esperar "llegó una muestra a metrics_raw" hay_metricas metrics_raw
esperar "llegaron sus interfaces a metrics_raw_networks" hay_metricas metrics_raw_networks
verde "ok: discos guardados: $(metricas_de metrics_raw_disks) (en un contenedor son los archivos que monta Docker)"
esperar "la ingesta agregó el minuto en metrics_1m" hay_metricas metrics_1m
# La tasa de red necesita dos muestras seguidas: llega con el segundo lote.
esperar "hay tasas de red en metrics_1m_networks" hay_metricas metrics_1m_networks

# --- 5. Reconexión ---

paso "reiniciando el control plane para forzar una reconexión"
ANTES_DEL_CORTE="$(psql_query "SELECT last_seen_at FROM servers WHERE name = '${SERVER_NAME}'")"
MUESTRAS_ANTES_DEL_CORTE="$(metricas_de metrics_raw)"

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

esperar "las métricas siguen llegando después de reconectar" \
  hay_metricas metrics_raw "${MUESTRAS_ANTES_DEL_CORTE}"

# --- 6. API del panel ---

paso "consultando la API del panel con la sesión"

LISTA="$("${COMPOSE[@]}" exec -T server wget -q -O- --header "Cookie: ${SESION}" http://127.0.0.1:8080/v1/servers)"
case "${LISTA}" in
  *"\"name\":\"${SERVER_NAME}\""*) ;;
  *) rojo "/v1/servers no trae al servidor: ${LISTA}"; exit 1 ;;
esac
case "${LISTA}" in
  *'"status":"online"'*) verde "ok: el panel ve el servidor en línea" ;;
  *) rojo "el servidor no figura en línea: ${LISTA}"; exit 1 ;;
esac
case "${LISTA}" in
  *'"cpu_used_percent"'*) verde "ok: la lista trae la última muestra" ;;
  *) rojo "la lista no trae la última muestra: ${LISTA}"; exit 1 ;;
esac

ID_SERVIDOR="$(psql_query "SELECT id FROM servers WHERE name = '${SERVER_NAME}'" | tr -d '[:space:]')"
SERIES="$("${COMPOSE[@]}" exec -T server wget -q -O- --header "Cookie: ${SESION}" \
  "http://127.0.0.1:8080/v1/servers/${ID_SERVIDOR}/metrics")"
case "${SERIES}" in
  *'"resolution":"raw"'*'"samples":[{'*) verde "ok: las series del servidor llegan con muestras" ;;
  *) rojo "las series salieron vacías o con otra forma: ${SERIES}"; exit 1 ;;
esac

# Sin la cookie, la API del panel no contesta.
if "${COMPOSE[@]}" exec -T server wget -q -O- http://127.0.0.1:8080/v1/servers >/dev/null 2>&1; then
  rojo "/v1/servers contestó sin sesión"
  exit 1
fi
verde "ok: sin sesión, la API del panel no contesta"

paso "resumen"
psql_query "SELECT name || ' | agent ' || agent_version || ' | último latido hace ' || round(extract(epoch from now() - last_seen_at)) || 's' FROM servers"
psql_query "SELECT 'métricas guardadas: ' || count(*) || ' muestras' FROM metrics_raw"

verde "la prueba de punta a punta pasó"
