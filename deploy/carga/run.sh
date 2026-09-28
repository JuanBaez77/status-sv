#!/usr/bin/env bash
#
# Prueba de carga: diez agents simulados más uno de verdad contra el control
# plane, y las metas de rendimiento de docs/DISENO.md verificadas al final.
#
#   RAM del control plane con 10 agents   menos de 100 MB
#   RAM del agent                          menos de 30 MB
#   CPU del agent                          menos de 1%
#
# Los agents simulados usan el cliente de verdad (deploy/../agent/cmd/loadtest):
# se enrolan, mantienen el canal, mandan lotes y esperan los acuses. El agent de
# verdad corre en su contenedor y mide su propio consumo, que viaja en el
# latido: de ahí salen las dos metas del agent.
#
#   ./deploy/carga/run.sh              # 2 minutos, lo que corre en cada PR
#   SEGUNDOS=600 ./deploy/carga/run.sh # la medición larga, la del workflow nocturno
#
# En Windows hay que anteponer MSYS_NO_PATHCONV=1, como en la prueba de punta a
# punta.
set -euo pipefail

cd "$(dirname "$0")/../.."

COMPOSE=(docker compose -f deploy/docker-compose.e2e.yml)
GO="${GO:-go}"

AGENTS="${AGENTS:-10}"
SEGUNDOS="${SEGUNDOS:-120}"
# Cada cuánto se mira la memoria del control plane mientras corre la prueba.
MUESTREO_SEGUNDOS=5

# Metas de docs/DISENO.md.
MAX_CONTROL_PLANE_MB=100
MAX_AGENT_MB=30
MAX_AGENT_CPU=1

# El agent de verdad, el que mide su propio consumo.
AGENT_NAME="vps-carga"

# El compose interpola el archivo entero en cada comando, así que la variable
# tiene que existir desde el principio. Se reemplaza por el token de verdad al
# levantar el agent.
export STATUSSV_ENROLL_TOKEN="${STATUSSV_ENROLL_TOKEN:-todavia-no-hay-token}"

# La clave que cifra los secretos TOTP, solo para esta corrida. El contenedor
# corre con otro usuario, así que tiene que poder leerla.
STATUSSV_TOTP_KEY_PATH="$(mktemp)"
openssl rand -base64 32 >"${STATUSSV_TOTP_KEY_PATH}"
chmod 644 "${STATUSSV_TOTP_KEY_PATH}"
# En Git Bash (Windows), Docker necesita la ruta en formato de Windows.
if command -v cygpath >/dev/null 2>&1; then
  STATUSSV_TOTP_KEY_PATH="$(cygpath -m "${STATUSSV_TOTP_KEY_PATH}")"
fi
export STATUSSV_TOTP_KEY_PATH

TRABAJO="$(mktemp -d)"
MEDICIONES="${TRABAJO}/rss.txt"
TOKENS="${TRABAJO}/tokens.txt"
MUESTREADOR=""

rojo() { printf '\033[31m%s\033[0m\n' "$*" >&2; }
verde() { printf '\033[32m%s\033[0m\n' "$*"; }
paso() { printf '\n=== %s ===\n' "$*"; }

limpiar() {
  local salida=$?
  if [ -n "${MUESTREADOR}" ]; then
    kill "${MUESTREADOR}" 2>/dev/null || true
  fi
  if [ $salida -ne 0 ]; then
    rojo "la prueba de carga falló; últimos registros del control plane:"
    "${COMPOSE[@]}" logs --tail 30 server 2>&1 || true
  fi
  paso "limpiando"
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  rm -f "${STATUSSV_TOTP_KEY_PATH}"
  rm -rf "${TRABAJO}"
  exit $salida
}
trap limpiar EXIT

psql_query() {
  "${COMPOSE[@]}" exec -T postgres psql -U statussv -d statussv -tAc "$1" | tr -d '[:space:]'
}

esperar() {
  local descripcion="$1"
  shift
  local fin=$((SECONDS + 120))
  until "$@" >/dev/null 2>&1; do
    if [ $SECONDS -ge $fin ]; then
      rojo "se agotó el tiempo esperando: ${descripcion}"
      return 1
    fi
    sleep 1
  done
  verde "ok: ${descripcion}"
}

# rss_control_plane devuelve la memoria residente del control plane, en kB. Es
# lo que el proceso ocupa de verdad en RAM, que es la meta del documento.
rss_control_plane() {
  "${COMPOSE[@]}" exec -T server sh -c 'grep VmRSS /proc/1/status' | awk '{print $2}'
}

# token_para genera un token de enrolamiento para un servidor.
token_para() {
  "${COMPOSE[@]}" exec -T server /usr/local/bin/server enroll-token --server "$1" \
    | grep -o 'svenr_[A-Za-z0-9_-]*' | head -1
}

# --- 1. Base y control plane ---

paso "levantando PostgreSQL y el control plane"
"${COMPOSE[@]}" up -d --build postgres server

esperar "el control plane responde /readyz" \
  "${COMPOSE[@]}" exec -T server wget -q -O- http://127.0.0.1:8080/readyz

paso "aplicando las migraciones"
"${COMPOSE[@]}" exec -T server /usr/local/bin/server migrate up

# --- 2. Agents ---

paso "generando ${AGENTS} tokens para los agents simulados"
: > "${TOKENS}"
for i in $(seq 1 "${AGENTS}"); do
  token_para "$(printf 'carga-%02d' "$i")" >> "${TOKENS}"
done
if [ "$(grep -c . "${TOKENS}")" != "${AGENTS}" ]; then
  rojo "no se generaron los ${AGENTS} tokens"
  exit 1
fi
verde "ok: ${AGENTS} tokens generados"

paso "levantando el agent de verdad"
STATUSSV_ENROLL_TOKEN="$(token_para "${AGENT_NAME}")"
export STATUSSV_ENROLL_TOKEN
"${COMPOSE[@]}" up -d --build agent

esperar "el agent de verdad está en línea" \
  bash -c "[ \"\$(${COMPOSE[*]} exec -T postgres psql -U statussv -d statussv -tAc \"SELECT count(*) FROM servers WHERE name = '${AGENT_NAME}' AND last_seen_at IS NOT NULL\" | tr -d '[:space:]')\" = '1' ]"

# --- 3. Carga ---

paso "midiendo con ${AGENTS} agents simulados durante ${SEGUNDOS}s"

# La memoria se mira mientras corre la prueba, no solo al final: el pico es lo
# que importa para la meta.
(
  while true; do
    rss_control_plane >> "${MEDICIONES}" 2>/dev/null || true
    sleep "${MUESTREO_SEGUNDOS}"
  done
) &
MUESTREADOR=$!

"${GO}" run ./agent/cmd/loadtest \
  --url http://127.0.0.1:8098 \
  --agents "${AGENTS}" \
  --duration "${SEGUNDOS}s" < "${TOKENS}"

kill "${MUESTREADOR}" 2>/dev/null || true
MUESTREADOR=""

# --- 4. Metas ---

paso "verificando las metas de rendimiento"

if [ ! -s "${MEDICIONES}" ]; then
  rojo "no se pudo medir la memoria del control plane"
  exit 1
fi

CONTROL_PLANE_KB="$(sort -n "${MEDICIONES}" | tail -1)"
CONTROL_PLANE_MB=$((CONTROL_PLANE_KB / 1024))

# El consumo del agent viaja en su latido y el control plane lo registra: de ahí
# salen las dos metas del agent, medidas por el agent mismo.
LATIDOS="$("${COMPOSE[@]}" logs server 2>/dev/null | grep 'msg=latido' | grep "servidor=${AGENT_NAME}" || true)"
if [ -z "${LATIDOS}" ]; then
  rojo "no llegó ningún latido del agent de verdad: no se pueden verificar sus metas"
  exit 1
fi

AGENT_KB="$(printf '%s\n' "${LATIDOS}" \
  | sed -n 's/.*memoria_agent_bytes=\([0-9]*\).*/\1/p' | sort -n | tail -1)"
AGENT_MB=$((AGENT_KB / 1024 / 1024))

# La primera medición de CPU es siempre 0 (no hay intervalo contra el cual
# comparar), así que se descarta para no bajar el promedio de arriba.
AGENT_CPU="$(printf '%s\n' "${LATIDOS}" \
  | sed -n 's/.*cpu_agent_porcentaje=\([0-9.]*\).*/\1/p' \
  | awk 'NR>1 {suma+=$1; n++} END {if (n>0) printf "%.3f", suma/n; else print "0"}')"
LATIDOS_CONTADOS="$(printf '%s\n' "${LATIDOS}" | grep -c .)"

MUESTRAS="$(psql_query "SELECT count(*) FROM metrics_raw m JOIN servers s ON s.id = m.server_id WHERE s.name LIKE 'carga-%'")"
# Cada agent muestrea cada 10 s y la primera lectura es solo la base.
ESPERADAS=$(( AGENTS * (SEGUNDOS / 10 - 1) ))
MINIMO=$(( ESPERADAS * 80 / 100 ))

paso "resumen"
printf '  RAM del control plane   %s MB (meta: menos de %s)\n' "${CONTROL_PLANE_MB}" "${MAX_CONTROL_PLANE_MB}"
printf '  RAM del agent           %s MB (meta: menos de %s)\n' "${AGENT_MB}" "${MAX_AGENT_MB}"
printf '  CPU del agent           %s%% promedio de %s latidos (meta: menos de %s%%)\n' \
  "${AGENT_CPU}" "${LATIDOS_CONTADOS}" "${MAX_AGENT_CPU}"
printf '  muestras en la base     %s de %s esperadas\n' "${MUESTRAS}" "${ESPERADAS}"

FALLA=0
if [ "${CONTROL_PLANE_MB}" -ge "${MAX_CONTROL_PLANE_MB}" ]; then
  rojo "el control plane usó ${CONTROL_PLANE_MB} MB con ${AGENTS} agents"
  FALLA=1
fi
if [ "${AGENT_MB}" -ge "${MAX_AGENT_MB}" ]; then
  rojo "el agent usó ${AGENT_MB} MB"
  FALLA=1
fi
if awk "BEGIN {exit !(${AGENT_CPU} >= ${MAX_AGENT_CPU})}"; then
  rojo "el agent promedió ${AGENT_CPU}% de CPU"
  FALLA=1
fi
if [ "${MUESTRAS}" -lt "${MINIMO}" ]; then
  rojo "llegaron ${MUESTRAS} muestras a la base y se esperaban al menos ${MINIMO}"
  FALLA=1
fi

if [ "${FALLA}" -ne 0 ]; then
  exit 1
fi

verde "la prueba de carga pasó: las metas de rendimiento se cumplen"
