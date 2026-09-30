#!/usr/bin/env bash
#
# Metas del panel: levanta el control plane con el panel adentro y un agent de
# verdad, y mide con deploy/metas/medir.mjs las dos metas de docs/DISENO.md que
# faltaba verificar:
#
#   Carga inicial del panel         menos de 1 s en conexión normal (Lighthouse)
#   Latencia métrica → pantalla     menos de 2 s
#
# Corre con los binarios compilados acá mismo, sin contenedores: el agent y el
# navegador tienen que compartir el reloj, y así se usa el mismo camino en CI y
# en una máquina de desarrollo. Hace falta:
#
#   DATABASE_URL   una base vacía de PostgreSQL (en CI, el servicio del job)
#   CHROME_PATH    el Chrome o Chromium que usan Lighthouse y la medición
#
#   DATABASE_URL=postgres://... CHROME_PATH=/usr/bin/google-chrome ./deploy/metas/run.sh
#
set -euo pipefail

cd "$(dirname "$0")/../.."

: "${DATABASE_URL:?hace falta DATABASE_URL, una base vacía para la medición}"
: "${CHROME_PATH:?hace falta CHROME_PATH, el Chrome o Chromium para medir}"
export DATABASE_URL CHROME_PATH

GO="${GO:-go}"
PUERTO="${PUERTO:-8097}"
URL="http://127.0.0.1:${PUERTO}"
SERVER_NAME="vps-metas"
USUARIO="metas"
CLAVE="clave-de-la-medicion-de-metas"

TRABAJO="$(mktemp -d)"
SERVER_PID=""
AGENT_PID=""

rojo() { printf '\033[31m%s\033[0m\n' "$*" >&2; }
verde() { printf '\033[32m%s\033[0m\n' "$*"; }
paso() { printf '\n=== %s ===\n' "$*"; }

limpiar() {
  local salida=$?
  for pid in "${AGENT_PID}" "${SERVER_PID}"; do
    if [ -n "${pid}" ]; then kill "${pid}" 2>/dev/null || true; fi
  done
  wait 2>/dev/null || true
  if [ $salida -ne 0 ]; then
    rojo "la medición falló; últimos registros:"
    tail -n 30 "${TRABAJO}/server.log" "${TRABAJO}/agent.log" 2>/dev/null || true
  fi
  rm -rf "${TRABAJO}"
  exit $salida
}
trap limpiar EXIT

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

# --- 1. Binarios, con el panel adentro como en producción ---

paso "compilando el panel y los binarios"
make web-embed >/dev/null
"${GO}" build -trimpath -o "${TRABAJO}/" ./server/cmd/server ./agent/cmd/agent
SERVER="${TRABAJO}/server"
AGENT="${TRABAJO}/agent"

# --- 2. Control plane ---

paso "levantando el control plane"
openssl rand -base64 32 >"${TRABAJO}/totp.key"
chmod 600 "${TRABAJO}/totp.key"
export STATUSSV_TOTP_KEY_FILE="${TRABAJO}/totp.key"
export STATUSSV_LISTEN_ADDR="127.0.0.1:${PUERTO}"
export STATUSSV_LOG_FORMAT=text

"${SERVER}" migrate up
"${SERVER}" serve >"${TRABAJO}/server.log" 2>&1 &
SERVER_PID=$!
esperar "el control plane responde /readyz" curl -fsS "${URL}/readyz"

printf '%s\n' "${CLAVE}" | "${SERVER}" user-add --username "${USUARIO}" >/dev/null
TOKEN="$("${SERVER}" enroll-token --server "${SERVER_NAME}" | grep -o 'svenr_[A-Za-z0-9_-]*' | head -1)"
if [ -z "${TOKEN}" ]; then
  rojo "el control plane no devolvió ningún token"
  exit 1
fi

# --- 3. Agent ---

paso "levantando el agent"
export STATUSSV_STATE_FILE="${TRABAJO}/agent.json"
export STATUSSV_PROJECTS_DIR="${TRABAJO}/projects.d"
STATUSSV_SERVER_URL="${URL}" STATUSSV_ENROLL_TOKEN="${TOKEN}" "${AGENT}" enroll >/dev/null
"${AGENT}" run >"${TRABAJO}/agent.log" 2>&1 &
AGENT_PID=$!

ID_SERVIDOR="$(psql "${DATABASE_URL}" -tAc "SELECT id FROM servers WHERE name = '${SERVER_NAME}'" | tr -d '[:space:]')"
hay_muestra() {
  [ "$(psql "${DATABASE_URL}" -tAc "SELECT count(*) FROM metrics_raw WHERE server_id = '${ID_SERVIDOR}'" | tr -d '[:space:]')" -gt 0 ]
}
esperar "llegó la primera muestra del agent" hay_muestra

# --- 4. Medición ---

paso "midiendo"
(cd deploy/metas && npm ci --no-audit --no-fund >/dev/null)
STATUSSV_URL="${URL}" STATUSSV_USER="${USUARIO}" STATUSSV_PASSWORD="${CLAVE}" \
  STATUSSV_SERVER_ID="${ID_SERVIDOR}" node deploy/metas/medir.mjs
