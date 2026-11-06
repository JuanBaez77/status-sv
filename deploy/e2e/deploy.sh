#!/usr/bin/env bash
#
# Deploy de punta a punta (ADR-015): el control plane, PostgreSQL, un agent de
# verdad con el deploy prendido y el Docker de la máquina, desplegando un
# proyecto Compose de muestra que vive en un repo file://.
#
#   1. Un deploy bueno de la punta de la rama, pedido por la API como lo pide
#      el panel. Mide la meta de inicio (menos de 3 s de la orden al primer
#      paso).
#   2. Un deploy roto (el healthcheck no pasa): vuelve atrás solo y el sitio
#      sigue con la versión anterior. Mientras corre, un segundo pedido se
#      rechaza.
#   3. Un rollback manual al estado bueno anterior.
#   4. El agent se reinicia a mitad de un build: al arrancar vuelve atrás.
#   5. El primer deploy de un proyecto que todavía no corre (ADR-021): pide un
#      código de la app aunque sea staging, clona en un workdir sin clonar sin
#      pisar el .env que dejó una persona, baja lo que levantó si no queda
#      bien y, una vez que quedó bien, lo que sigue son deploys.
#   6. Todo quedó en audit_log.
#   7. El MCP de lectura (ADR-024, ADR-025), con tokens creados como en el
#      panel: las siete herramientas sobre lo que dejaron los pasos
#      anteriores, un secreto sembrado en el log de un contenedor que llega
#      enmascarado, los rechazos (sin token, inventado, desde un navegador,
#      sin el scope, revocado) y cada llamada en audit_log como llm.
#
# Corre con los binarios compilados acá, sin contenedores para el agent: tiene
# que ver el Docker de la máquina como en un servidor. Hace falta:
#
#   DATABASE_URL   una base vacía de PostgreSQL (en CI, el servicio del job)
#   HEALTH_HOST    desde dónde se ven los puertos publicados por Docker
#                  (127.0.0.1 en CI; host.docker.internal si esto corre
#                  adentro de un contenedor con Docker Desktop)
#
#   DATABASE_URL=postgres://... ./deploy/e2e/deploy.sh
#
set -euo pipefail

cd "$(dirname "$0")/../.."

: "${DATABASE_URL:?hace falta DATABASE_URL, una base vacía para la prueba}"
export DATABASE_URL
HEALTH_HOST="${HEALTH_HOST:-127.0.0.1}"

GO="${GO:-go}"
PUERTO="${PUERTO:-8096}"
PUERTO_WEB="${PUERTO_WEB:-18091}"
URL="http://127.0.0.1:${PUERTO}"
SERVER_NAME="vps-deploy-e2e"
USUARIO="deploy"
CLAVE="clave-de-la-prueba-de-deploy"
PROYECTO="tienda"
COMPOSE_PROJECT="statussv-e2e-deploy"
# El proyecto que todavía no corre: se levanta con un primer deploy.
NUEVO="turnos"
NUEVO_COMPOSE="statussv-e2e-bootstrap"
PUERTO_NUEVO="${PUERTO_NUEVO:-18093}"
# INICIO_MAX es la meta de docs/DISENO.md: inicio de un deploy en menos de 3 s.
INICIO_MAX_MS=3000

TRABAJO="$(mktemp -d)"
ORIGEN="${TRABAJO}/origen.git"
FUENTE="${TRABAJO}/fuente"
WD="${TRABAJO}/proyecto"
ORIGEN_NUEVO="${TRABAJO}/origen-nuevo.git"
FUENTE_NUEVO="${TRABAJO}/fuente-nuevo"
WD_NUEVO="${TRABAJO}/nuevo"
SERVER_PID=""
AGENT_PID=""

rojo() { printf '\033[31m%s\033[0m\n' "$*" >&2; }
verde() { printf '\033[32m%s\033[0m\n' "$*"; }
paso() { printf '\n=== %s ===\n' "$*"; }

compose_wd() {
  docker compose --project-directory "${WD}" -f "${WD}/compose.yml" -p "${COMPOSE_PROJECT}" "$@"
}
compose_nuevo() {
  docker compose --project-directory "${WD_NUEVO}" -f "${WD_NUEVO}/compose.yml" -p "${NUEVO_COMPOSE}" "$@"
}

limpiar() {
  local salida=$?
  for pid in "${AGENT_PID}" "${SERVER_PID}"; do
    if [ -n "${pid}" ]; then kill "${pid}" 2>/dev/null || true; fi
  done
  wait 2>/dev/null || true
  if [ $salida -ne 0 ]; then
    rojo "la prueba falló; últimos registros:"
    tail -n 40 "${TRABAJO}/server.log" "${TRABAJO}"/agent*.log 2>/dev/null || true
  fi
  [ -d "${WD}" ] && compose_wd down --rmi local --volumes >/dev/null 2>&1 || true
  [ -f "${WD_NUEVO}/compose.yml" ] && compose_nuevo down --rmi local --volumes >/dev/null 2>&1 || true
  for proyecto in "${COMPOSE_PROJECT}" "${NUEVO_COMPOSE}"; do
    for ref in $(docker images --format '{{.Repository}}:{{.Tag}}' "statussv-rollback/${proyecto}/*" 2>/dev/null); do
      docker image rm "${ref}" >/dev/null 2>&1 || true
    done
  done
  rm -rf "${TRABAJO}"
  exit $salida
}
trap limpiar EXIT

esperar() {
  local descripcion="$1" limite="$2"
  shift 2
  local fin=$((SECONDS + limite))
  until "$@" >/dev/null 2>&1; do
    if [ $SECONDS -ge $fin ]; then
      rojo "se agotó el tiempo esperando: ${descripcion}"
      return 1
    fi
    sleep 1
  done
  verde "ok: ${descripcion}"
}

falla() {
  rojo "FALLA: $*"
  exit 1
}

# --- El proyecto de muestra ---

G=(git -c user.name=e2e -c user.email=e2e@status-sv.invalid -c init.defaultBranch=main)

# version escribe una versión del sitio y la commitea. health=no deja al sitio
# sin /health (el healthcheck falla); lento=si hace que el build tarde.
version() {
  local nombre="$1" health="${2:-si}" lento="${3:-no}"
  rm -rf "${FUENTE}/www"
  mkdir -p "${FUENTE}/www"
  echo "${nombre}" >"${FUENTE}/www/version"
  [ "${health}" = si ] && echo ok >"${FUENTE}/www/health"
  [ "${lento}" = si ] && echo si >"${FUENTE}/www/lento"
  "${G[@]}" -C "${FUENTE}" add -A
  "${G[@]}" -C "${FUENTE}" commit -qm "${nombre}"
  "${G[@]}" -C "${FUENTE}" push -q origin main
  git -C "${FUENTE}" rev-parse HEAD
}

paso "armando el proyecto de muestra"
"${G[@]}" init -q --bare "${ORIGEN}"
"${G[@]}" init -q "${FUENTE}"
"${G[@]}" -C "${FUENTE}" remote add origin "${ORIGEN}"
printf 'FROM %s/busybox:1.37\n' "${IMAGENES:-docker.io/library}" >"${FUENTE}/Dockerfile"
cat >>"${FUENTE}/Dockerfile" <<'EOF'
COPY www/ /www/
# Una versión "lenta" tarda en compilar, para poder cortar al agent en el medio.
RUN if [ -f /www/lento ]; then sleep 40; fi
CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]
EOF
cat >"${FUENTE}/compose.yml" <<EOF
services:
  web:
    build: .
    ports:
      - "127.0.0.1:${PUERTO_WEB}:8080"
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/health"]
      interval: 1s
      timeout: 2s
      retries: 20
EOF
V1="$(version v1)"
V2="$(version v2)"

# El workdir lo clonó una persona y está corriendo v1, como en un servidor.
git clone -q "file://${ORIGEN}" "${WD}"
git -C "${WD}" checkout -q -B main "${V1}"
compose_wd up -d --build >/dev/null 2>&1
sirve() { [ "$(curl -fsS "http://${HEALTH_HOST}:${PUERTO_WEB}/version" 2>/dev/null)" = "$1" ]; }
esperar "el sitio de muestra sirve v1" 60 sirve v1

mkdir -p "${TRABAJO}/projects.d" "${TRABAJO}/keys"
cat >"${TRABAJO}/projects.d/${PROYECTO}.yaml" <<EOF
name: ${PROYECTO}
environment: staging
repo: file://${ORIGEN}
branch: main
runtime: docker
compose_file: compose.yml
compose_project: ${COMPOSE_PROJECT}
workdir: ${WD}
healthcheck:
  url: http://${HEALTH_HOST}:${PUERTO_WEB}/health
  expect_status: 200
  timeout_seconds: 2
deploy:
  pre:
    - service: web
      command: ["cat", "/www/version"]
EOF

# El proyecto nuevo: declarado, con el workdir que creó una persona y su .env
# adentro, pero sin clonar y sin nada corriendo. Su primera versión no tiene
# /health, así que el primer intento no queda bien.
"${G[@]}" init -q --bare "${ORIGEN_NUEVO}"
"${G[@]}" init -q "${FUENTE_NUEVO}"
"${G[@]}" -C "${FUENTE_NUEVO}" remote add origin "${ORIGEN_NUEVO}"
printf 'FROM %s/busybox:1.37\n' "${IMAGENES:-docker.io/library}" >"${FUENTE_NUEVO}/Dockerfile"
cat >>"${FUENTE_NUEVO}/Dockerfile" <<'EOF'
COPY www/ /www/
CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]
EOF
cat >"${FUENTE_NUEVO}/compose.yml" <<EOF
services:
  web:
    build: .
    environment:
      SALUDO: \${SALUDO:?falta el .env del workdir}
    ports:
      - "127.0.0.1:${PUERTO_NUEVO}:8080"
EOF
version_nuevo() {
  local nombre="$1" health="${2:-si}"
  rm -rf "${FUENTE_NUEVO}/www"
  mkdir -p "${FUENTE_NUEVO}/www"
  echo "${nombre}" >"${FUENTE_NUEVO}/www/version"
  [ "${health}" = si ] && echo ok >"${FUENTE_NUEVO}/www/health"
  "${G[@]}" -C "${FUENTE_NUEVO}" add -A
  "${G[@]}" -C "${FUENTE_NUEVO}" commit -qm "${nombre}"
  "${G[@]}" -C "${FUENTE_NUEVO}" push -q origin main
  git -C "${FUENTE_NUEVO}" rev-parse HEAD
}
N1="$(version_nuevo n1 no)"
mkdir -p "${WD_NUEVO}"
echo 'SALUDO=hola' >"${WD_NUEVO}/.env"
cat >"${TRABAJO}/projects.d/${NUEVO}.yaml" <<EOF
name: ${NUEVO}
environment: staging
repo: file://${ORIGEN_NUEVO}
branch: main
runtime: docker
compose_file: compose.yml
compose_project: ${NUEVO_COMPOSE}
workdir: ${WD_NUEVO}
healthcheck:
  url: http://${HEALTH_HOST}:${PUERTO_NUEVO}/health
  expect_status: 200
  timeout_seconds: 2
EOF

# --- Control plane y agent ---

paso "compilando y levantando el control plane"
"${GO}" build -trimpath -o "${TRABAJO}/" ./server/cmd/server ./agent/cmd/agent
SERVER="${TRABAJO}/server"
AGENT="${TRABAJO}/agent"

openssl rand -base64 32 >"${TRABAJO}/totp.key"
chmod 600 "${TRABAJO}/totp.key"
export STATUSSV_TOTP_KEY_FILE="${TRABAJO}/totp.key"
export STATUSSV_LISTEN_ADDR="127.0.0.1:${PUERTO}"
export STATUSSV_LOG_FORMAT=text
"${SERVER}" migrate up >/dev/null
"${SERVER}" serve >"${TRABAJO}/server.log" 2>&1 &
SERVER_PID=$!
esperar "el control plane responde /readyz" 60 curl -fsS "${URL}/readyz"

printf '%s\n' "${CLAVE}" | "${SERVER}" user-add --username "${USUARIO}" >/dev/null
TOKEN="$("${SERVER}" enroll-token --server "${SERVER_NAME}" | grep -o 'svenr_[A-Za-z0-9_-]*' | head -1)"
[ -n "${TOKEN}" ] || falla "el control plane no devolvió ningún token"

paso "levantando el agent con el deploy prendido"
export STATUSSV_STATE_FILE="${TRABAJO}/estado/agent.json"
export STATUSSV_PROJECTS_DIR="${TRABAJO}/projects.d"
export STATUSSV_DEPLOY_KEYS_DIR="${TRABAJO}/keys"
export STATUSSV_KNOWN_HOSTS="${TRABAJO}/known_hosts"
export STATUSSV_DEPLOY_ENABLED=true
export STATUSSV_DEPLOY_ALLOW_FILE_REPOS=true
export STATUSSV_LOG_LEVEL=debug
STATUSSV_SERVER_URL="${URL}" STATUSSV_ENROLL_TOKEN="${TOKEN}" "${AGENT}" enroll >/dev/null
levantar_agent() {
  "${AGENT}" run >>"${TRABAJO}/agent$1.log" 2>&1 &
  AGENT_PID=$!
}
levantar_agent 1

# --- Sesión del panel, con el segundo factor ---

paso "iniciando sesión en el panel"
# totp_code calcula el código TOTP (RFC 6238) de un secreto en base32 para un
# paso de 30 s, como la app del teléfono.
totp_code() {
  local clave paso hmac offset valor
  clave="$(printf '%s' "$1" | base32 -d | od -An -tx1 | tr -d ' \n')"
  paso="$(printf '%016x' "$2")"
  hmac="$(printf '%s' "${paso}" | sed 's/../\\x&/g' | xargs -0 printf '%b' \
    | openssl dgst -sha1 -mac HMAC -macopt "hexkey:${clave}" -r | cut -c1-40)"
  offset=$((16#${hmac:39:1}))
  valor=$(((16#${hmac:$((offset * 2)):8} & 0x7fffffff) % 1000000))
  printf '%06d' "${valor}"
}
# cookie_de saca la cookie de sesión de los encabezados de una respuesta.
cookie_de() { grep -io '__Host-statussv_session=svses_[A-Za-z0-9_-]*' | head -1; }

SESION="$(curl -sS -D - -o /dev/null -H 'Content-Type: application/json' \
  --data "{\"username\":\"${USUARIO}\",\"password\":\"${CLAVE}\"}" "${URL}/v1/auth/login" | cookie_de)"
[ -n "${SESION}" ] || falla "el login no devolvió la cookie"
SECRETO="$(curl -sS -X POST -H "Cookie: ${SESION}" "${URL}/v1/auth/totp/enroll" | jq -r .secret)"
PASO_USADO=$(($(date +%s) / 30))
CODIGO="$(totp_code "${SECRETO}" "${PASO_USADO}")"
SESION="$(curl -sS -D - -o /dev/null -H "Cookie: ${SESION}" -H 'Content-Type: application/json' \
  --data "{\"code\":\"${CODIGO}\"}" "${URL}/v1/auth/totp/confirm" | cookie_de)"
[ -n "${SESION}" ] || falla "el alta del segundo factor no devolvió la sesión completa"
verde "ok: sesión completa"

# codigo_nuevo deja en CODIGO un código que todavía no se usó: el control
# plane no acepta el mismo dos veces, así que espera al paso de 30 s que
# sigue, como haría una persona con la app.
codigo_nuevo() {
  while [ $(($(date +%s) / 30)) -le "${PASO_USADO}" ]; do sleep 1; done
  PASO_USADO=$(($(date +%s) / 30))
  CODIGO="$(totp_code "${SECRETO}" "${PASO_USADO}")"
}

api() { curl -sS -H "Cookie: ${SESION}" -H 'Content-Type: application/json' "$@"; }

proyecto_id() { api "${URL}/v1/projects" | jq -r ".projects[] | select(.name == \"${1:-${PROYECTO}}\") | .id"; }
declarado() { [ -n "$(proyecto_id "$@")" ]; }
esperar "el agent declaró el proyecto" 60 declarado
ID_PROYECTO="$(proyecto_id)"

# pedir pide un deploy, un rollback o un primer deploy y deja el cuerpo de la
# respuesta en ${TRABAJO}/respuesta.json. Devuelve el código HTTP. El tercer
# argumento es el proyecto; sin él, el de muestra.
pedir() {
  local tipo="$1" cuerpo="$2" proyecto="${3:-${ID_PROYECTO}}"
  api -o "${TRABAJO}/respuesta.json" -w '%{http_code}' --data "${cuerpo}" "${URL}/v1/projects/${proyecto}/${tipo}"
}

estado() { api "${URL}/v1/deploys/$1" | jq -r .status; }
termino() { case "$(estado "$1")" in succeeded | failed | rolled_back) return 0 ;; *) return 1 ;; esac }

# desplegar pide, mide el inicio y espera el final. Deja el ID en DEPLOY.
desplegar() {
  local tipo="$1" cuerpo="$2" limite="$3"
  local inicio fin codigo
  inicio=$(date +%s%3N)
  codigo="$(pedir "${tipo}" "${cuerpo}")"
  [ "${codigo}" = 201 ] || falla "el pedido devolvió ${codigo}: $(cat "${TRABAJO}/respuesta.json")"
  DEPLOY="$(jq -r .id "${TRABAJO}/respuesta.json")"
  until [ "$(estado "${DEPLOY}")" != pending ]; do
    [ $(($(date +%s%3N) - inicio)) -lt 10000 ] || falla "el agent no empezó el deploy en 10 s"
    sleep 0.05
  done
  fin=$(date +%s%3N)
  INICIO_MS=$((fin - inicio))
  esperar "terminó el ${tipo%s} ${DEPLOY}" "${limite}" termino "${DEPLOY}"
}

detalle() { api "${URL}/v1/deploys/$1"; }

# --- 1. Deploy bueno ---

paso "1. deploy de la punta de la rama (v2)"
desplegar deploys "{\"confirm\":\"${PROYECTO}\"}" 180
DETALLE="$(detalle "${DEPLOY}")"
[ "$(jq -r .status <<<"${DETALLE}")" = succeeded ] || falla "el deploy no terminó bien: $(jq -c '{status,code,message}' <<<"${DETALLE}")"
[ "$(jq -r .commit <<<"${DETALLE}")" = "${V2}" ] || falla "se desplegó otro commit"
[ "$(jq -r .previous_commit <<<"${DETALLE}")" = "${V1}" ] || falla "el commit anterior no es v1"
PASOS="$(jq -r '[.steps[] | .name + ":" + .status] | join(" ")' <<<"${DETALLE}")"
[ "${PASOS}" = "preflight:ok fetch:ok snapshot:ok checkout:ok build:ok pre:ok up:ok healthcheck:ok" ] || falla "pasos: ${PASOS}"
jq -e '.steps[] | select(.name == "pre") | .output | index("v2")' <<<"${DETALLE}" >/dev/null || falla "el paso pre no corrió con la imagen nueva"
sirve v2 || falla "el sitio no sirve v2"
verde "ok: desplegado v2 en $(jq -r '.steps | length' <<<"${DETALLE}") pasos"
[ "${INICIO_MS}" -lt "${INICIO_MAX_MS}" ] || falla "el deploy tardó ${INICIO_MS} ms en empezar (meta: ${INICIO_MAX_MS})"
verde "ok: el deploy empezó en ${INICIO_MS} ms (meta: menos de ${INICIO_MAX_MS})"
INICIO_DEPLOY="${INICIO_MS}"
git -C "${WD}" rev-parse HEAD | grep -qx "${V2}" || falla "el workdir no quedó en v2"
[ "$(git -C "${WD}" rev-parse --abbrev-ref HEAD)" = main ] || falla "el workdir no quedó en la rama main"

# --- 2. Deploy roto ---

paso "2. deploy roto (v3 sin /health): vuelve atrás solo"
V3="$(version v3 no)"
codigo="$(pedir deploys "{\"confirm\":\"${PROYECTO}\"}")"
[ "${codigo}" = 201 ] || falla "el pedido devolvió ${codigo}"
ROTO="$(jq -r .id "${TRABAJO}/respuesta.json")"
# Mientras corre, un segundo pedido se rechaza.
codigo="$(pedir deploys "{\"confirm\":\"${PROYECTO}\"}")"
[ "${codigo}" = 409 ] || falla "el segundo pedido devolvió ${codigo} y no 409"
jq -e '.error == "deploy_in_progress"' "${TRABAJO}/respuesta.json" >/dev/null || falla "el segundo pedido no dijo deploy_in_progress"
verde "ok: un segundo pedido mientras corre se rechaza"
esperar "terminó el deploy roto" 240 termino "${ROTO}"
DETALLE="$(detalle "${ROTO}")"
[ "$(jq -r .status <<<"${DETALLE}")" = rolled_back ] || falla "el deploy roto terminó $(jq -c '{status,code,message}' <<<"${DETALLE}")"
[ "$(jq -r .code <<<"${DETALLE}")" = unhealthy ] || falla "código $(jq -r .code <<<"${DETALLE}")"
[ "$(jq -r '.steps[-1].name + ":" + .steps[-1].status' <<<"${DETALLE}")" = "rollback:ok" ] || falla "no hay paso de rollback bueno"
sirve v2 || falla "después de volver atrás el sitio no sirve v2"
git -C "${WD}" rev-parse HEAD | grep -qx "${V2}" || falla "el workdir no volvió a v2"
verde "ok: volvió atrás solo y el sitio sigue en v2 (${V3:0:8} no quedó)"

# --- 3. Rollback manual ---

paso "3. rollback manual al estado bueno anterior (v1)"
desplegar rollback "{\"confirm\":\"${PROYECTO}\"}" 120
DETALLE="$(detalle "${DEPLOY}")"
[ "$(jq -r .status <<<"${DETALLE}")" = succeeded ] || falla "el rollback terminó $(jq -c '{status,code,message}' <<<"${DETALLE}")"
[ "$(jq -r .commit <<<"${DETALLE}")" = "${V1}" ] || falla "el rollback no volvió a v1"
jq -e '.steps[] | select(.name == "build" and .status == "skipped")' <<<"${DETALLE}" >/dev/null || falla "el rollback compiló"
sirve v1 || falla "el sitio no sirve v1"
verde "ok: volvió a v1 sin compilar"

# --- 4. Agent reiniciado en medio de un build ---

paso "4. el agent se reinicia a mitad de un build"
V4="$(version v4 si si)"
codigo="$(pedir deploys "{\"confirm\":\"${PROYECTO}\"}")"
[ "${codigo}" = 201 ] || falla "el pedido devolvió ${codigo}"
CORTADO="$(jq -r .id "${TRABAJO}/respuesta.json")"
compilando() { api "${URL}/v1/deploys/${CORTADO}" | jq -e '.steps[-1].name == "build" and .steps[-1].status == "running"'; }
esperar "el deploy está compilando" 60 compilando
# Como systemd al reiniciar el servicio: SIGTERM al agent.
kill -TERM "${AGENT_PID}"
wait "${AGENT_PID}" 2>/dev/null || true
AGENT_PID=""
[ "$(estado "${CORTADO}")" = running ] || falla "el deploy cortado no quedó en curso"
levantar_agent 2
esperar "el agent volvió atrás el deploy cortado" 120 termino "${CORTADO}"
DETALLE="$(detalle "${CORTADO}")"
[ "$(jq -r .status <<<"${DETALLE}")" = rolled_back ] || falla "el deploy cortado terminó $(jq -c '{status,code,message}' <<<"${DETALLE}")"
[ "$(jq -r .code <<<"${DETALLE}")" = interrupted ] || falla "código $(jq -r .code <<<"${DETALLE}")"
sirve v1 || falla "el sitio no sirve v1 después del reinicio"
git -C "${WD}" rev-parse HEAD | grep -qx "${V1}" || falla "el workdir no volvió a v1"
verde "ok: al arrancar volvió atrás el deploy cortado (${V4:0:8} no quedó)"

# --- 5. Primer deploy ---

paso "5. primer deploy de un proyecto que todavía no corre"
esperar "el agent declaró el proyecto nuevo" 60 declarado "${NUEVO}"
ID_NUEVO="$(proyecto_id "${NUEVO}")"
resumen() { jq -c '{kind,status,code,message}' <<<"$1"; }
pasos() { jq -r '[.steps[] | .name + ":" + .status] | join(" ")' <<<"$1"; }
sirve_nuevo() { [ "$(curl -fsS "http://${HEALTH_HOST}:${PUERTO_NUEVO}/version" 2>/dev/null)" = "$1" ]; }
contenedores_nuevo() { docker ps -aq --filter "label=com.docker.compose.project=${NUEVO_COMPOSE}"; }

# Un deploy común no alcanza: el workdir todavía no tiene el código.
codigo="$(pedir deploys "{\"confirm\":\"${NUEVO}\"}" "${ID_NUEVO}")"
[ "${codigo}" = 201 ] || falla "el pedido devolvió ${codigo}"
SIN_CLONAR="$(jq -r .id "${TRABAJO}/respuesta.json")"
esperar "terminó el deploy común del proyecto sin clonar" 60 termino "${SIN_CLONAR}"
DETALLE="$(detalle "${SIN_CLONAR}")"
[ "$(jq -r .code <<<"${DETALLE}")" = preflight_failed ] || falla "el deploy sin clonar terminó $(resumen "${DETALLE}")"
jq -e '.message | test("primer deploy")' <<<"${DETALLE}" >/dev/null || falla "el mensaje no dice qué hacer: $(jq -r .message <<<"${DETALLE}")"
[ ! -e "${WD_NUEVO}/.git" ] || falla "un deploy común inició el repositorio"
verde "ok: un deploy común dice que lo que va es un primer deploy, sin tocar nada"

# El primer deploy pide el código de la app siempre, también en staging.
codigo="$(pedir bootstrap "{\"confirm\":\"${NUEVO}\"}" "${ID_NUEVO}")"
[ "${codigo}" = 403 ] || falla "el primer deploy sin código devolvió ${codigo} y no 403"
jq -e '.error == "invalid_code"' "${TRABAJO}/respuesta.json" >/dev/null || falla "el primer deploy sin código no dijo invalid_code"
verde "ok: sin un código nuevo de la app no se pide"

# El primer intento no queda bien (n1 no tiene /health): baja lo que levantó.
codigo_nuevo
codigo="$(pedir bootstrap "{\"confirm\":\"${NUEVO}\",\"totp_code\":\"${CODIGO}\"}" "${ID_NUEVO}")"
[ "${codigo}" = 201 ] || falla "el primer deploy devolvió ${codigo}: $(cat "${TRABAJO}/respuesta.json")"
INTENTO="$(jq -r .id "${TRABAJO}/respuesta.json")"
esperar "terminó el primer deploy que no queda bien" 300 termino "${INTENTO}"
DETALLE="$(detalle "${INTENTO}")"
[ "$(jq -r '.kind + " " + .status + " " + .code' <<<"${DETALLE}")" = "bootstrap rolled_back unhealthy" ] || falla "el primer intento terminó $(resumen "${DETALLE}")"
[ "$(pasos "${DETALLE}")" = "preflight:ok fetch:ok checkout:ok build:ok up:ok healthcheck:failed rollback:ok" ] || falla "pasos: $(pasos "${DETALLE}")"
[ "$(jq -r '.previous_commit // ""' <<<"${DETALLE}")" = "" ] || falla "un primer deploy no tiene commit anterior"
[ -z "$(contenedores_nuevo)" ] || falla "quedaron contenedores después de bajar lo que levantó"
git -C "${WD_NUEVO}" rev-parse HEAD | grep -qx "${N1}" || falla "el código clonado no quedó en el workdir"
[ "$(cat "${WD_NUEVO}/.env")" = 'SALUDO=hola' ] || falla "el primer deploy pisó el .env del workdir"
verde "ok: no quedó bien, bajó lo que levantó y dejó el código clonado y el .env como estaban"

# Con una versión que anda, queda: el intento anterior no lo traba.
N2="$(version_nuevo n2)"
codigo_nuevo
codigo="$(pedir bootstrap "{\"confirm\":\"${NUEVO}\",\"totp_code\":\"${CODIGO}\"}" "${ID_NUEVO}")"
[ "${codigo}" = 201 ] || falla "el segundo primer deploy devolvió ${codigo}: $(cat "${TRABAJO}/respuesta.json")"
PRIMERO="$(jq -r .id "${TRABAJO}/respuesta.json")"
esperar "terminó el primer deploy" 180 termino "${PRIMERO}"
DETALLE="$(detalle "${PRIMERO}")"
[ "$(jq -r '.kind + " " + .status' <<<"${DETALLE}")" = "bootstrap succeeded" ] || falla "el primer deploy terminó $(resumen "${DETALLE}")"
[ "$(jq -r .commit <<<"${DETALLE}")" = "${N2}" ] || falla "el primer deploy no es de la punta de la rama"
[ "$(pasos "${DETALLE}")" = "preflight:ok fetch:ok checkout:ok build:ok up:ok healthcheck:ok" ] || falla "pasos: $(pasos "${DETALLE}")"
jq -e '.steps[0].detail | test("intento anterior")' <<<"${DETALLE}" >/dev/null || falla "el chequeo previo no reconoció el clon del intento anterior"
sirve_nuevo n2 || falla "el proyecto nuevo no sirve n2"
# El .env que dejó una persona en el workdir llegó al contenedor.
[ "$(compose_nuevo exec -T web sh -c 'echo "${SALUDO}"')" = hola ] || falla "el contenedor no recibió el .env del workdir"
verde "ok: el proyecto quedó levantado en n2, con el .env del workdir"

# Ya corre: otro primer deploy termina sin tocar nada.
codigo_nuevo
codigo="$(pedir bootstrap "{\"confirm\":\"${NUEVO}\",\"totp_code\":\"${CODIGO}\"}" "${ID_NUEVO}")"
[ "${codigo}" = 201 ] || falla "el pedido devolvió ${codigo}"
REPETIDO="$(jq -r .id "${TRABAJO}/respuesta.json")"
esperar "terminó el primer deploy repetido" 60 termino "${REPETIDO}"
DETALLE="$(detalle "${REPETIDO}")"
[ "$(jq -r '.status + " " + .code' <<<"${DETALLE}")" = "failed already_deployed" ] || falla "el primer deploy repetido terminó $(resumen "${DETALLE}")"
sirve_nuevo n2 || falla "el primer deploy repetido tocó lo que corría"
verde "ok: un proyecto que ya corre no acepta otro primer deploy"

# Y lo que sigue son deploys, con su foto de lo anterior.
N3="$(version_nuevo n3)"
codigo="$(pedir deploys "{\"confirm\":\"${NUEVO}\"}" "${ID_NUEVO}")"
[ "${codigo}" = 201 ] || falla "el pedido devolvió ${codigo}"
SIGUIENTE="$(jq -r .id "${TRABAJO}/respuesta.json")"
esperar "terminó el deploy que sigue al primero" 180 termino "${SIGUIENTE}"
DETALLE="$(detalle "${SIGUIENTE}")"
[ "$(jq -r .status <<<"${DETALLE}")" = succeeded ] || falla "el deploy que sigue terminó $(resumen "${DETALLE}")"
[ "$(jq -r '.commit + " " + .previous_commit' <<<"${DETALLE}")" = "${N3} ${N2}" ] || falla "el deploy que sigue no partió del primer deploy"
sirve_nuevo n3 || falla "el proyecto nuevo no sirve n3"
verde "ok: después del primer deploy, un deploy común anda"

# --- 6. Auditoría ---

paso "6. auditoría"
contar() { psql "${DATABASE_URL}" -tAc "$1" | tr -d '[:space:]'; }
PEDIDOS="$(contar "SELECT count(*) FROM audit_log WHERE action IN ('deploy.request','rollback.request') AND actor = '${USUARIO}' AND actor_type = 'user'")"
PRIMEROS="$(contar "SELECT count(*) FROM audit_log WHERE action = 'bootstrap.request' AND actor = '${USUARIO}' AND actor_type = 'user'")"
FINALES="$(contar "SELECT count(*) FROM audit_log WHERE action = 'deploy.finish' AND actor = '${SERVER_NAME}' AND actor_type = 'agent'")"
RECHAZOS="$(contar "SELECT count(*) FROM audit_log WHERE action IN ('deploy.request','bootstrap.request') AND result = 'rejected'")"
# Del proyecto de muestra, cuatro pedidos aceptados y uno rechazado; del nuevo,
# dos deploys, y cuatro primeros deploys con uno rechazado por no traer código.
# Nueve finales: todos los que el agent llegó a empezar.
[ "${PEDIDOS}" = 7 ] || falla "hay ${PEDIDOS} pedidos en la auditoría y se esperaban 7"
[ "${PRIMEROS}" = 4 ] || falla "hay ${PRIMEROS} pedidos de primer deploy en la auditoría y se esperaban 4"
[ "${FINALES}" = 9 ] || falla "hay ${FINALES} finales en la auditoría y se esperaban 9"
[ "${RECHAZOS}" = 2 ] || falla "hay ${RECHAZOS} rechazos en la auditoría y se esperaban 2"
if psql "${DATABASE_URL}" -tAc "DELETE FROM audit_log" >/dev/null 2>&1; then
  falla "se pudo borrar audit_log"
fi
verde "ok: ${PEDIDOS} pedidos, ${PRIMEROS} primeros deploys, ${FINALES} finales y ${RECHAZOS} rechazos en audit_log, que no se puede borrar"

# --- 7. MCP de lectura ---

paso "7. el MCP de lectura, con un token del panel (ADR-024, ADR-025)"
# crear_token crea un token del MCP como lo crea el panel, con un código nuevo
# de la app, y deja la respuesta en ${TRABAJO}/token.json.
crear_token() {
  local nombre="$1" scopes="$2" codigo
  codigo_nuevo
  codigo="$(api -o "${TRABAJO}/token.json" -w '%{http_code}' --data \
    "{\"name\":\"${nombre}\",\"scopes\":${scopes},\"project_ids\":[],\"expires_in_days\":1,\"totp_code\":\"${CODIGO}\"}" \
    "${URL}/v1/tokens")"
  [ "${codigo}" = 201 ] || falla "crear el token ${nombre} devolvió ${codigo}: $(cat "${TRABAJO}/token.json")"
}
crear_token e2e-claude '["read:staging","logs:staging"]'
MCP_TOKEN="$(jq -r .token "${TRABAJO}/token.json")"
ID_TOKEN="$(jq -r .item.id "${TRABAJO}/token.json")"
[[ "${MCP_TOKEN}" == svmcp_* ]] || falla "el token no tiene el prefijo svmcp_"
crear_token e2e-produccion '["read:production"]'
TOKEN_PRODUCCION="$(jq -r .token "${TRABAJO}/token.json")"
verde "ok: dos tokens creados con un código nuevo cada uno"

# mcp llama una herramienta como Claude Code: JSON-RPC por HTTP, sin estado,
# así que no hace falta initialize. Deja la respuesta en ${TRABAJO}/mcp.json y
# devuelve el código HTTP. Los demás argumentos son encabezados extra.
MCP_ID=0
mcp() {
  local token="$1" metodo="$2" params="$3"
  shift 3
  MCP_ID=$((MCP_ID + 1))
  curl -sS -o "${TRABAJO}/mcp.json" -w '%{http_code}' "$@" \
    -H "Authorization: Bearer ${token}" -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' -H 'MCP-Protocol-Version: 2025-06-18' \
    --data "{\"jsonrpc\":\"2.0\",\"id\":${MCP_ID},\"method\":\"${metodo}\",\"params\":${params}}" \
    "${URL}/mcp"
}
# herramienta llama una herramienta con el token del e2e y devuelve su salida
# estructurada; falla si la herramienta contestó con un error.
herramienta() {
  local codigo
  codigo="$(mcp "${MCP_TOKEN}" tools/call "{\"name\":\"$1\",\"arguments\":${2:-"{}"}}")"
  [ "${codigo}" = 200 ] || falla "$1 devolvió ${codigo}: $(cat "${TRABAJO}/mcp.json")"
  jq -e '.result.isError != true' "${TRABAJO}/mcp.json" >/dev/null || falla "$1 falló: $(jq -c '.result.content' "${TRABAJO}/mcp.json")"
  jq -c '.result.structuredContent' "${TRABAJO}/mcp.json"
}
error_de() { jq -r '.result.content[0].text // .error.message // ""' "${TRABAJO}/mcp.json"; }

codigo="$(mcp "${MCP_TOKEN}" tools/list '{}')"
[ "${codigo}" = 200 ] || falla "tools/list devolvió ${codigo}"
HERRAMIENTAS="$(jq -r '[.result.tools[].name] | sort | join(" ")' "${TRABAJO}/mcp.json")"
[ "${HERRAMIENTAS}" = "deploy get_approval get_deploy get_logs get_project_status get_server_metrics list_deploys list_projects list_servers restart rollback" ] || falla "herramientas: ${HERRAMIENTAS}"
# Las tres escrituras (ADR-026) van marcadas como tales; las demás, de solo lectura.
jq -e '[.result.tools[] | select(.name | IN("deploy", "rollback", "restart") | not) | .annotations.readOnlyHint] | all' "${TRABAJO}/mcp.json" >/dev/null || falla "hay lecturas que no son de solo lectura"
jq -e '[.result.tools[] | select(.name | IN("deploy", "rollback", "restart")) | .annotations.destructiveHint] | all' "${TRABAJO}/mcp.json" >/dev/null || falla "hay escrituras que no están marcadas"
verde "ok: once herramientas: ocho de lectura y tres escrituras marcadas"

SALIDA="$(herramienta list_servers)"
[ "$(jq -r ".servers[] | select(.name == \"${SERVER_NAME}\") | .projects | sort | join(\",\")" <<<"${SALIDA}")" = "${PROYECTO},${NUEVO}" ] || falla "list_servers: ${SALIDA}"
SALIDA="$(herramienta get_server_metrics "{\"server\":\"${SERVER_NAME}\",\"range\":\"1h\"}")"
jq -e '.samples > 0 and .resolution == "raw" and .cpu_percent.max >= 0 and (.series.timestamps | length) > 0' <<<"${SALIDA}" >/dev/null || falla "get_server_metrics: ${SALIDA}"
SALIDA="$(herramienta list_projects)"
[ "$(jq -r '[.projects[].ref] | sort | join(" ")' <<<"${SALIDA}")" = "${SERVER_NAME}/${PROYECTO} ${SERVER_NAME}/${NUEVO}" ] || falla "list_projects: ${SALIDA}"
verde "ok: servidores, métricas y proyectos"

# La versión que corre sale de la historia: el último deploy (el cortado)
# volvió atrás a v1.
SALIDA="$(herramienta get_project_status "{\"project\":\"${PROYECTO}\"}")"
[ "$(jq -r .version.commit <<<"${SALIDA}")" = "${V1}" ] || falla "versión: $(jq -c .version <<<"${SALIDA}")"
[ "$(jq -r .last_deploy.id <<<"${SALIDA}")" = "${CORTADO}" ] || falla "último deploy: $(jq -c .last_deploy <<<"${SALIDA}")"
jq -e '(.containers | length) >= 1 and .runtime_source == "ok"' <<<"${SALIDA}" >/dev/null || falla "get_project_status: ${SALIDA}"
SALIDA="$(herramienta list_deploys "{\"project\":\"${SERVER_NAME}/${PROYECTO}\",\"limit\":2}")"
[ "$(jq -r '[.deploys[].id] | join(" ")' <<<"${SALIDA}")" = "${CORTADO} ${DEPLOY}" ] || falla "list_deploys: ${SALIDA}"
SALIDA="$(herramienta get_deploy "{\"deploy_id\":\"${ROTO}\"}")"
[ "$(jq -r '.deploy.status + " " + .deploy.code' <<<"${SALIDA}")" = "rolled_back unhealthy" ] || falla "get_deploy: $(jq -c .deploy <<<"${SALIDA}")"
jq -e '(.steps[] | select(.name == "healthcheck") | .status) == "failed" and (.untrusted_notice | test("no confiable"))' <<<"${SALIDA}" >/dev/null || falla "los pasos del deploy roto: ${SALIDA}"
verde "ok: estado, versión (${V1:0:8}), historia y pasos de un deploy"

# Un secreto en el log del contenedor sale enmascarado del agent.
compose_wd exec -T web sh -c 'echo "conectando a postgres://tienda:supersecreto@db/tienda" >/proc/1/fd/1'
SALIDA="$(herramienta get_logs "{\"project\":\"${PROYECTO}\",\"unit\":\"${COMPOSE_PROJECT}-web-1\",\"lines\":20}")"
jq -e '[.lines[].text] | any(test("postgres://tienda:\\*\\*\\*@db"))' <<<"${SALIDA}" >/dev/null || falla "el renglón sembrado no llegó enmascarado: ${SALIDA}"
! grep -q supersecreto <<<"${SALIDA}" || falla "el secreto llegó al MCP"
jq -e '.untrusted_notice | test("no confiable")' <<<"${SALIDA}" >/dev/null || falla "los logs no traen el aviso"
verde "ok: get_logs trae el log del contenedor con el secreto enmascarado"

# Los rechazos.
codigo="$(mcp "" tools/list '{}')"
[ "${codigo}" = 401 ] || falla "sin token devolvió ${codigo}"
codigo="$(mcp "svmcp_inventadoinventadoinventadoinventadoinvent" tools/list '{}')"
[ "${codigo}" = 401 ] || falla "un token inventado devolvió ${codigo}"
codigo="$(mcp "${MCP_TOKEN}" tools/list '{}' -H 'Origin: https://evil.example')"
[ "${codigo}" = 403 ] || falla "con Origin devolvió ${codigo}"
codigo="$(mcp "${TOKEN_PRODUCCION}" tools/call "{\"name\":\"get_project_status\",\"arguments\":{\"project\":\"${PROYECTO}\"}}")"
[ "${codigo}" = 200 ] && jq -e '.result.isError' "${TRABAJO}/mcp.json" >/dev/null || falla "read:production con un proyecto de staging: ${codigo} $(cat "${TRABAJO}/mcp.json")"
error_de | grep -q "no hay un proyecto" || falla "el error no es el de un proyecto que no alcanza: $(error_de)"
codigo="$(mcp "${TOKEN_PRODUCCION}" tools/call "{\"name\":\"get_logs\",\"arguments\":{\"project\":\"${PROYECTO}\",\"unit\":\"web\"}}")"
error_de | grep -q "no tiene permiso de logs" || falla "sin logs: $(error_de)"
codigo="$(api -o /dev/null -w '%{http_code}' -X DELETE "${URL}/v1/tokens/${ID_TOKEN}")"
[ "${codigo}" = 200 ] || falla "revocar el token devolvió ${codigo}"
codigo="$(mcp "${MCP_TOKEN}" tools/list '{}')"
[ "${codigo}" = 401 ] || falla "un token revocado devolvió ${codigo}"
verde "ok: sin token, inventado, desde un navegador, sin el scope y revocado"

# La auditoría tiene cada llamada, con el token como actor de tipo llm, y en
# ningún lado quedaron ni el secreto ni el valor del token.
LLAMADAS="$(contar "SELECT count(*) FROM audit_log WHERE actor = 'e2e-claude' AND actor_type = 'llm' AND action LIKE 'mcp.%' AND result = 'ok'")"
[ "${LLAMADAS}" = 7 ] || falla "hay ${LLAMADAS} llamadas buenas del token en la auditoría y se esperaban 7 (tools/list no es una llamada)"
DENEGADAS="$(contar "SELECT count(*) FROM audit_log WHERE actor = 'e2e-produccion' AND result IN ('not_found','denied')")"
[ "${DENEGADAS}" = 2 ] || falla "hay ${DENEGADAS} llamadas rechazadas del token de producción y se esperaban 2"
[ "$(contar "SELECT count(*) FROM audit_log WHERE action = 'mcp.auth' AND result = 'rejected'")" = 1 ] || falla "los tokens que no sirven se anotan una vez por IP"
[ "$(contar "SELECT count(*) FROM audit_log WHERE action IN ('token.create','token.revoke') AND actor = '${USUARIO}'")" = 3 ] || falla "faltan la creación o la revocación de los tokens en la auditoría"
[ "$(contar "SELECT count(*) FROM audit_log WHERE detail::text LIKE '%supersecreto%' OR detail::text LIKE '%${MCP_TOKEN}%'")" = 0 ] || falla "la auditoría guardó el secreto o el token"
! grep -qF -e supersecreto -e "${MCP_TOKEN}" "${TRABAJO}/server.log" || falla "los registros del control plane tienen el secreto o el token"
[ "$(contar "SELECT count(*) FROM api_tokens WHERE token_hash = sha256('${MCP_TOKEN}'::bytea)")" = 1 ] || falla "la base no tiene el hash del token"
[ "$(contar "SELECT count(*) FROM api_tokens WHERE prefix = '${MCP_TOKEN:0:12}'")" = 1 ] || falla "la base no tiene el prefijo del token"
verde "ok: ${LLAMADAS} llamadas en audit_log como llm; ni el secreto ni el token quedaron en la base ni en los registros"

paso "listo"
verde "el deploy de punta a punta anduvo (inicio en ${INICIO_DEPLOY} ms)"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### Deploy de punta a punta"
    echo
    echo "| Meta | Medido | Límite |"
    echo "| --- | --- | --- |"
    echo "| Inicio de un deploy | ${INICIO_DEPLOY} ms | ${INICIO_MAX_MS} ms |"
  } >>"${GITHUB_STEP_SUMMARY}"
fi
