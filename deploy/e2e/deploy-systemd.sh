#!/usr/bin/env bash
#
# Deploy de systemd de punta a punta (ADR-019): el control plane, PostgreSQL y
# un agent instalado de verdad, con install-agent.sh, corriendo bajo su unidad
# endurecida, con el ayudante root y un proyecto que corre como servicio de
# systemd, en un repo file://.
#
#   1. Un deploy bueno de la punta de la rama, pedido por la API como lo pide
#      el panel: build, pre, reinicio y verificación. Los pasos corren con el
#      usuario del proyecto, y su salida llega al control plane.
#   2. Un deploy roto (el servicio se cae al arrancar): vuelve atrás solo y el
#      sitio sigue con la versión anterior.
#   3. Un paso build que falla: vuelve atrás sin reiniciar el servicio.
#   4. Un rollback manual al estado bueno anterior.
#   5. El agent se reinicia a mitad de un paso: el paso se para y, al arrancar,
#      el agent vuelve atrás.
#   6. Todo quedó en audit_log.
#
# Se corre como root en una máquina descartable con systemd como PID 1: el
# runner del CI o WSL. Instala y saca lo suyo (el agent, el ayudante, usuarios
# y unidades de prueba). Hace falta:
#
#   DATABASE_URL   una base vacía de PostgreSQL
#   SERVER_BIN     el binario del control plane para Linux
#   AGENT_BIN      el binario del agent para Linux
#
#   sudo DATABASE_URL=postgres://... SERVER_BIN=... AGENT_BIN=... ./deploy/e2e/deploy-systemd.sh
#
set -euo pipefail
# Un comando que falla adentro de $(...) también corta la prueba: sin esto, un
# git push fallido en version() pasaba de largo.
shopt -s inherit_errexit

cd "$(dirname "$0")/../.."
RAIZ="$(pwd)"

: "${DATABASE_URL:?hace falta DATABASE_URL, una base vacía para la prueba}"
: "${SERVER_BIN:?hace falta SERVER_BIN, el binario del control plane}"
: "${AGENT_BIN:?hace falta AGENT_BIN, el binario del agent}"
export DATABASE_URL

PUERTO="${PUERTO:-8097}"
PUERTO_WEB="${PUERTO_WEB:-18092}"
URL="http://127.0.0.1:${PUERTO}"
SERVER_NAME="vps-systemd-e2e"
USUARIO="deploy"
CLAVE="clave-de-la-prueba-de-deploy"
PROYECTO="tienda-systemd"
UNIDAD="e2e-tienda.service"
APP_USER="e2eapp"
ORIGEN=/srv/e2e-systemd-origen.git
WD=/srv/e2e-systemd
DROPIN_DIR=/etc/systemd/system/statussv-agent.service.d

TRABAJO="$(mktemp -d)"
FUENTE="${TRABAJO}/fuente"
SERVER_PID=""
MARCA_USUARIO="${TRABAJO}/creo-statussv"

rojo() { printf '\033[31m%s\033[0m\n' "$*" >&2; }
verde() { printf '\033[32m%s\033[0m\n' "$*"; }
paso() { printf '\n=== %s ===\n' "$*"; }

[ "$(id -u)" -eq 0 ] || { rojo "hay que correrlo como root"; exit 2; }
for p in systemctl git curl jq openssl base32 python3 psql; do
  command -v "$p" >/dev/null || { rojo "falta ${p}"; exit 2; }
done
[ -x "${SERVER_BIN}" ] && [ -x "${AGENT_BIN}" ] || { rojo "SERVER_BIN y AGENT_BIN tienen que ser binarios para Linux"; exit 2; }
if [ -e /etc/systemd/system/statussv-agent.service ] || [ -e /etc/status-sv ]; then
  rojo "esta máquina ya tiene un agent de status-sv instalado: la prueba lo pisaría"
  exit 2
fi

limpiar() {
  local salida=$?
  set +e
  if [ $salida -ne 0 ]; then
    rojo "la prueba falló; últimos registros:"
    tail -n 30 "${TRABAJO}/server.log" 2>/dev/null
    journalctl -u statussv-agent -n 40 --no-pager -o cat 2>/dev/null
    journalctl -u 'statussv-unit-helper@*' -n 20 --no-pager -o cat 2>/dev/null
    journalctl -u "${UNIDAD}" -n 15 --no-pager -o cat 2>/dev/null
  fi
  [ -n "${SERVER_PID}" ] && kill "${SERVER_PID}" 2>/dev/null
  wait 2>/dev/null
  systemctl disable --now statussv-agent statussv-unit-helper.socket "${UNIDAD}" >/dev/null 2>&1
  systemctl stop 'statussv-unit-helper@*.service' 'statussv-step-*.service' 2>/dev/null
  systemctl reset-failed statussv-agent "${UNIDAD}" 'statussv-unit-helper@*.service' 'statussv-step-*.service' 2>/dev/null
  rm -rf /etc/systemd/system/statussv-agent.service "${DROPIN_DIR}" "/etc/systemd/system/${UNIDAD}" \
    /etc/systemd/system/statussv-unit-helper.socket /etc/systemd/system/statussv-unit-helper@.service
  systemctl daemon-reload
  rm -rf "${WD}" "${ORIGEN}" /etc/status-sv /var/lib/status-sv /run/status-sv /usr/local/bin/statussv-agent
  userdel "${APP_USER}" 2>/dev/null
  [ -f "${MARCA_USUARIO}" ] && userdel statussv 2>/dev/null
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

# El origen termina siendo del usuario del agent, y root le sigue empujando
# versiones. Git no escribe en un repo de otro dueño si no se lo declara de
# confianza, y eso lo lee quien recibe el push: tiene que venir de un archivo
# de configuración, no de un -c. Va en uno propio de la prueba, sin tocar el
# de root.
printf '[safe]\n\tdirectory = %s\n' "${ORIGEN}" >"${TRABAJO}/gitconfig"
G=(env "GIT_CONFIG_GLOBAL=${TRABAJO}/gitconfig" git -c user.name=e2e -c user.email=e2e@status-sv.invalid -c init.defaultBranch=main)

# version escribe una versión del sitio y la commitea. Las marcas cambian cómo
# se porta: "roto" hace que el servicio se caiga al arrancar, "build-falla" que
# el paso build termine mal, y "lento" que el build tarde.
version() {
  local nombre="$1" marca="${2:-}"
  rm -f "${FUENTE}/roto" "${FUENTE}/build-falla" "${FUENTE}/lento"
  echo "${nombre}" >"${FUENTE}/version"
  [ -n "${marca}" ] && echo si >"${FUENTE}/${marca}"
  "${G[@]}" -C "${FUENTE}" add -A
  "${G[@]}" -C "${FUENTE}" commit -qm "${nombre}"
  "${G[@]}" -C "${FUENTE}" push -q origin main
  # Lo que quedó en el origen, que es lo que el agent va a traer.
  "${G[@]}" -C "${ORIGEN}" rev-parse refs/heads/main
}

paso "armando el proyecto de muestra"
if ! id statussv >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin statussv
  touch "${MARCA_USUARIO}"
fi
useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "${APP_USER}"

"${G[@]}" init -q --bare "${ORIGEN}"
"${G[@]}" init -q "${FUENTE}"
"${G[@]}" -C "${FUENTE}" remote add origin "${ORIGEN}"
# El sitio: un servidor HTTP mínimo, como haría gunicorn. Sirve su versión y
# lo que dejó el paso build.
cat >"${FUENTE}/app.py" <<EOF
import http.server, os, sys

if os.path.exists("roto"):
    print("esta versión no arranca", flush=True)
    sys.exit(1)

def leer(ruta):
    try:
        return open(ruta).read()
    except OSError:
        return ""

class Sitio(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        cuerpo = {"/health": "ok\n", "/version": leer("version"), "/construido": leer("var/construido.txt")}.get(self.path)
        self.send_response(200 if cuerpo is not None else 404)
        self.end_headers()
        self.wfile.write((cuerpo or "").encode())

    def log_message(self, *args):
        pass

http.server.HTTPServer(("127.0.0.1", ${PUERTO_WEB}), Sitio).serve_forever()
EOF
# El paso build: lo que sería un pip install. Corre con el usuario del proyecto.
cat >"${FUENTE}/build.sh" <<'EOF'
#!/bin/sh
echo "preparando $(cat version) como $(id -un)"
echo "con PROYECTO_PASSWORD=${PROYECTO_PASSWORD:-sin-entorno}"
if [ -f lento ]; then sleep 60; fi
if [ -f build-falla ]; then echo "no se pudo instalar una dependencia"; exit 3; fi
echo "$(cat version) $(id -un)" > var/construido.txt
EOF
# El paso pre: lo que sería una migración.
cat >"${FUENTE}/migrar.sh" <<'EOF'
#!/bin/sh
echo "migrando a $(cat version)"
cat version >> var/migraciones.txt
EOF
chmod 0755 "${FUENTE}/build.sh" "${FUENTE}/migrar.sh"
V1="$(version v1)"
V2="$(version v2)"

# El workdir lo clonó root y corre v1, como en un servidor. El origen queda
# del usuario del agent: git no sirve por file:// un repo de otro dueño.
git clone -q "file://${ORIGEN}" "${WD}"
git -C "${WD}" checkout -q -B main "${V1}"
chown -R statussv "${ORIGEN}"
chgrp -R statussv "${WD}/.git"
chmod -R g+rwX "${WD}/.git"
find "${WD}/.git" -type d -exec chmod g+s {} +
chgrp statussv "${WD}" && chmod g+rwxs "${WD}"
# Lo que el usuario del proyecto escribe queda en un directorio suyo, fuera de git.
install -d -o "${APP_USER}" -g "${APP_USER}" -m 0755 "${WD}/var"
echo "v1 ${APP_USER}" >"${WD}/var/construido.txt"
chown "${APP_USER}:${APP_USER}" "${WD}/var/construido.txt"

# El archivo de entorno del proyecto: solo de root.
install -d -m 0755 /etc/status-sv
install -m 0600 /dev/null /etc/status-sv/e2e-tienda.env
echo 'PROYECTO_PASSWORD=hunter2-del-proyecto' >/etc/status-sv/e2e-tienda.env

cat >"/etc/systemd/system/${UNIDAD}" <<EOF
[Unit]
Description=Sitio de muestra de la prueba de deploy de systemd

[Service]
User=${APP_USER}
WorkingDirectory=${WD}
ExecStart=/usr/bin/python3 ${WD}/app.py
Restart=always
RestartSec=1
EOF
systemctl daemon-reload
systemctl start "${UNIDAD}"
sirve() { [ "$(curl -fsS "http://127.0.0.1:${PUERTO_WEB}/version" 2>/dev/null)" = "$1" ]; }
esperar "el sitio de muestra sirve v1" 30 sirve v1

install -d -o root -g statussv -m 0750 /etc/status-sv/projects.d
cat >"/etc/status-sv/projects.d/${PROYECTO}.yaml" <<EOF
name: ${PROYECTO}
environment: staging
repo: file://${ORIGEN}
branch: main
runtime: systemd
workdir: ${WD}
units:
  - ${UNIDAD}
run_as: ${APP_USER}
env_file: /etc/status-sv/e2e-tienda.env
healthcheck:
  url: http://127.0.0.1:${PUERTO_WEB}/health
  expect_status: 200
  timeout_seconds: 2
deploy:
  build:
    - command: ["./build.sh"]
      timeout_seconds: 120
  pre:
    - command: ["./migrar.sh"]
rollback: previous_commit
EOF
chown root:statussv "/etc/status-sv/projects.d/${PROYECTO}.yaml"
chmod 0640 "/etc/status-sv/projects.d/${PROYECTO}.yaml"

# --- Control plane ---

paso "levantando el control plane"
openssl rand -base64 32 >"${TRABAJO}/totp.key"
chmod 600 "${TRABAJO}/totp.key"
export STATUSSV_TOTP_KEY_FILE="${TRABAJO}/totp.key"
export STATUSSV_LISTEN_ADDR="127.0.0.1:${PUERTO}"
export STATUSSV_LOG_FORMAT=text
"${SERVER_BIN}" migrate up >/dev/null
"${SERVER_BIN}" serve >"${TRABAJO}/server.log" 2>&1 &
SERVER_PID=$!
esperar "el control plane responde /readyz" 60 curl -fsS "${URL}/readyz"

printf '%s\n' "${CLAVE}" | "${SERVER_BIN}" user-add --username "${USUARIO}" >/dev/null
TOKEN="$("${SERVER_BIN}" enroll-token --server "${SERVER_NAME}" | grep -o 'svenr_[A-Za-z0-9_-]*' | head -1)"
[ -n "${TOKEN}" ] || falla "el control plane no devolvió ningún token"

# --- El agent, instalado como en un servidor ---

paso "instalando el agent con --enable-deploy"
# Lo único que no es de un servidor de verdad: aceptar un repo file://.
install -d -m 0755 "${DROPIN_DIR}"
printf '[Service]\nEnvironment=STATUSSV_DEPLOY_ALLOW_FILE_REPOS=true\nEnvironment=STATUSSV_LOG_LEVEL=debug\n' >"${DROPIN_DIR}/30-e2e.conf"
if ! INSTALADOR="$(STATUSSV_SERVER_URL="${URL}" STATUSSV_ENROLL_TOKEN="${TOKEN}" STATUSSV_DEPLOY_ALLOW_FILE_REPOS=true \
  bash "${RAIZ}/deploy/install-agent.sh" --enable-deploy "${AGENT_BIN}" 2>&1)"; then
  printf '%s\n' "${INSTALADOR}"
  falla "el instalador falló"
fi
grep -q "${PROYECTO} se puede desplegar" <<<"${INSTALADOR}" || {
  printf '%s\n' "${INSTALADOR}" | sed -n '/revisando el deploy/,$p'
  falla "deploy-check no aprobó el proyecto"
}
systemctl is-active --quiet statussv-unit-helper.socket || falla "el instalador no dejó el ayudante"
verde "ok: agent instalado, ayudante activo y deploy-check aprobó el proyecto"

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
cookie_de() { grep -io '__Host-statussv_session=svses_[A-Za-z0-9_-]*' | head -1; }

SESION="$(curl -sS -D - -o /dev/null -H 'Content-Type: application/json' \
  --data "{\"username\":\"${USUARIO}\",\"password\":\"${CLAVE}\"}" "${URL}/v1/auth/login" | cookie_de)"
[ -n "${SESION}" ] || falla "el login no devolvió la cookie"
SECRETO="$(curl -sS -X POST -H "Cookie: ${SESION}" "${URL}/v1/auth/totp/enroll" | jq -r .secret)"
CODIGO="$(totp_code "${SECRETO}" $(($(date +%s) / 30)))"
SESION="$(curl -sS -D - -o /dev/null -H "Cookie: ${SESION}" -H 'Content-Type: application/json' \
  --data "{\"code\":\"${CODIGO}\"}" "${URL}/v1/auth/totp/confirm" | cookie_de)"
[ -n "${SESION}" ] || falla "el alta del segundo factor no devolvió la sesión completa"
verde "ok: sesión completa"

api() { curl -sS -H "Cookie: ${SESION}" -H 'Content-Type: application/json' "$@"; }

proyecto_id() { api "${URL}/v1/projects" | jq -r ".projects[] | select(.name == \"${PROYECTO}\") | .id"; }
declarado() { [ -n "$(proyecto_id)" ]; }
esperar "el agent declaró el proyecto" 90 declarado
ID_PROYECTO="$(proyecto_id)"

pedir() {
  local tipo="$1" cuerpo="$2"
  api -o "${TRABAJO}/respuesta.json" -w '%{http_code}' --data "${cuerpo}" "${URL}/v1/projects/${ID_PROYECTO}/${tipo}"
}
estado() { api "${URL}/v1/deploys/$1" | jq -r .status; }
termino() { case "$(estado "$1")" in succeeded | failed | rolled_back) return 0 ;; *) return 1 ;; esac }
detalle() { api "${URL}/v1/deploys/$1"; }
pasos() { jq -r '[.steps[] | .name + ":" + .status] | join(" ")' <<<"$1"; }
resumen() { jq -c '{status,code,message}' <<<"$1"; }

# desplegar pide y espera el final. Deja el ID en DEPLOY.
desplegar() {
  local tipo="$1" limite="$2" codigo
  codigo="$(pedir "${tipo}" "{\"confirm\":\"${PROYECTO}\"}")"
  [ "${codigo}" = 201 ] || falla "el pedido devolvió ${codigo}: $(cat "${TRABAJO}/respuesta.json")"
  DEPLOY="$(jq -r .id "${TRABAJO}/respuesta.json")"
  esperar "terminó el ${tipo%s} ${DEPLOY}" "${limite}" termino "${DEPLOY}"
}

pid_del_sitio() { systemctl show -p MainPID --value "${UNIDAD}"; }
construido() { curl -fsS "http://127.0.0.1:${PUERTO_WEB}/construido" 2>/dev/null; }
en_workdir() { git -c "safe.directory=${WD}" -C "${WD}" rev-parse HEAD; }

# --- 1. Deploy bueno ---

paso "1. deploy de la punta de la rama (v2)"
PID_ANTES="$(pid_del_sitio)"
desplegar deploys 180
DETALLE="$(detalle "${DEPLOY}")"
[ "$(jq -r .status <<<"${DETALLE}")" = succeeded ] || falla "el deploy no terminó bien: $(resumen "${DETALLE}")"
[ "$(jq -r .commit <<<"${DETALLE}")" = "${V2}" ] || falla "se desplegó otro commit"
[ "$(jq -r .previous_commit <<<"${DETALLE}")" = "${V1}" ] || falla "el commit anterior no es v1"
[ "$(pasos "${DETALLE}")" = "preflight:ok fetch:ok snapshot:ok checkout:ok build:ok pre:ok restart:ok healthcheck:ok" ] || falla "pasos: $(pasos "${DETALLE}")"
sirve v2 || falla "el sitio no sirve v2"
[ "$(pid_del_sitio)" != "${PID_ANTES}" ] || falla "el servicio no se reinició"
# El paso build corrió con el usuario del proyecto, no con el del agent.
[ "$(construido)" = "v2 ${APP_USER}" ] || falla "el paso build dejó \"$(construido)\""
# Su salida llegó al control plane, leída del diario.
jq -e --arg u "${APP_USER}" '.steps[] | select(.name == "build") | .output | index("preparando v2 como " + $u)' <<<"${DETALLE}" >/dev/null \
  || falla "la salida del paso build no llegó: $(jq -c '.steps[] | select(.name == "build") | .output' <<<"${DETALLE}")"
# El paso recibió el archivo de entorno, y lo que escribió salió enmascarado.
SALIDA_BUILD="$(jq -r '.steps[] | select(.name == "build") | .output | join("\n")' <<<"${DETALLE}")"
grep -q 'con PROYECTO_PASSWORD=' <<<"${SALIDA_BUILD}" || falla "el paso no dijo nada del entorno: ${SALIDA_BUILD}"
grep -q 'sin-entorno' <<<"${SALIDA_BUILD}" && falla "el paso no recibió el archivo de entorno"
grep -q 'hunter2-del-proyecto' <<<"${SALIDA_BUILD}" && falla "un secreto del proyecto llegó al control plane sin enmascarar"
[ "$(jq -r '.steps[] | select(.name == "restart") | .detail' <<<"${DETALLE}")" = "${UNIDAD}" ] || falla "el paso restart no dice qué reinició"
[ "$(en_workdir)" = "${V2}" ] || falla "el workdir no quedó en v2"
verde "ok: desplegado v2: build y pre como ${APP_USER}, reinicio y verificación"

# --- 2. Deploy roto ---

paso "2. deploy roto (v3 se cae al arrancar): vuelve atrás solo"
V3="$(version v3 roto)"
desplegar deploys 300
DETALLE="$(detalle "${DEPLOY}")"
[ "$(jq -r .status <<<"${DETALLE}")" = rolled_back ] || falla "el deploy roto terminó $(resumen "${DETALLE}")"
[ "$(jq -r .code <<<"${DETALLE}")" = unhealthy ] || falla "código $(jq -r .code <<<"${DETALLE}")"
[ "$(jq -r '.steps[-1].name + ":" + .steps[-1].status' <<<"${DETALLE}")" = "rollback:ok" ] || falla "no hay paso de rollback bueno: $(pasos "${DETALLE}")"
esperar "el sitio volvió a servir v2" 30 sirve v2
[ "$(en_workdir)" = "${V2}" ] || falla "el workdir no volvió a v2"
# Al volver atrás se corrió build de nuevo, sobre el código anterior.
[ "$(construido)" = "v2 ${APP_USER}" ] || falla "después de volver atrás, build dejó \"$(construido)\""
verde "ok: volvió atrás solo y el sitio sigue en v2 (${V3:0:8} no quedó)"

# --- 3. Un paso build que falla ---

paso "3. un paso build que falla (v4): vuelve atrás sin reiniciar"
V4="$(version v4 build-falla)"
PID_ANTES="$(pid_del_sitio)"
desplegar deploys 180
DETALLE="$(detalle "${DEPLOY}")"
[ "$(jq -r .status <<<"${DETALLE}")" = rolled_back ] || falla "terminó $(resumen "${DETALLE}")"
[ "$(jq -r .code <<<"${DETALLE}")" = build_failed ] || falla "código $(jq -r .code <<<"${DETALLE}")"
grep -q 'salió con código 3' <<<"$(jq -r .message <<<"${DETALLE}")" || falla "el mensaje no trae el código de salida: $(jq -r .message <<<"${DETALLE}")"
[ "$(pid_del_sitio)" = "${PID_ANTES}" ] || falla "el servicio se reinició aunque el build falló"
sirve v2 || falla "el sitio no sirve v2"
[ "$(en_workdir)" = "${V2}" ] || falla "el workdir no volvió a v2"
verde "ok: el build falló con código 3 y el servicio ni se tocó (${V4:0:8} no quedó)"

# --- 4. Rollback manual ---

paso "4. rollback manual al estado bueno anterior (v1)"
desplegar rollback 180
DETALLE="$(detalle "${DEPLOY}")"
[ "$(jq -r .status <<<"${DETALLE}")" = succeeded ] || falla "el rollback terminó $(resumen "${DETALLE}")"
[ "$(jq -r .commit <<<"${DETALLE}")" = "${V1}" ] || falla "el rollback no volvió a v1"
[ "$(pasos "${DETALLE}")" = "preflight:ok snapshot:ok checkout:ok build:ok restart:ok healthcheck:ok" ] || falla "pasos: $(pasos "${DETALLE}")"
sirve v1 || falla "el sitio no sirve v1"
[ "$(construido)" = "v1 ${APP_USER}" ] || falla "el rollback no preparó de nuevo el código: \"$(construido)\""
# Una migración no se deshace ni se repite: el paso pre corrió con v2 y con v3
# (que después volvió atrás), y el rollback no lo corre de nuevo.
[ "$(tr '\n' ' ' <"${WD}/var/migraciones.txt")" = "v2 v3 " ] || falla "migraciones: $(tr '\n' ' ' <"${WD}/var/migraciones.txt")"
verde "ok: volvió a v1 corriendo build de nuevo, sin repetir la migración"

# --- 5. Agent reiniciado en medio de un paso ---

paso "5. el agent se reinicia a mitad de un paso build"
V5="$(version v5 lento)"
codigo="$(pedir deploys "{\"confirm\":\"${PROYECTO}\"}")"
[ "${codigo}" = 201 ] || falla "el pedido devolvió ${codigo}"
CORTADO="$(jq -r .id "${TRABAJO}/respuesta.json")"
PASO_BUILD="statussv-step-${CORTADO}-build-0.service"
preparando() { [ "$(systemctl show -p ActiveState --value "${PASO_BUILD}")" = activating ]; }
esperar "el paso build está corriendo" 60 preparando
systemctl restart statussv-agent
# Sin el agent que lo esperaba, el ayudante paró el paso.
parado() { [ "$(systemctl show -p ActiveState --value "${PASO_BUILD}")" != activating ]; }
esperar "el paso se paró al irse el agent" 30 parado
esperar "el agent volvió atrás el deploy cortado" 120 termino "${CORTADO}"
DETALLE="$(detalle "${CORTADO}")"
[ "$(jq -r .status <<<"${DETALLE}")" = rolled_back ] || falla "el deploy cortado terminó $(resumen "${DETALLE}")"
[ "$(jq -r .code <<<"${DETALLE}")" = interrupted ] || falla "código $(jq -r .code <<<"${DETALLE}")"
sirve v1 || falla "el sitio no sirve v1 después del reinicio"
[ "$(en_workdir)" = "${V1}" ] || falla "el workdir no volvió a v1"
verde "ok: al arrancar volvió atrás el deploy cortado (${V5:0:8} no quedó)"

# --- 6. Auditoría ---

paso "6. auditoría"
contar() { psql "${DATABASE_URL}" -tAc "$1" | tr -d '[:space:]'; }
PEDIDOS="$(contar "SELECT count(*) FROM audit_log WHERE action IN ('deploy.request','rollback.request') AND actor = '${USUARIO}' AND result = 'accepted'")"
FINALES="$(contar "SELECT count(*) FROM audit_log WHERE action = 'deploy.finish' AND actor = '${SERVER_NAME}' AND actor_type = 'agent'")"
ENROLADO="$(contar "SELECT count(*) FROM audit_log WHERE action = 'agent.enroll' AND actor = '${SERVER_NAME}' AND result = 'ok'")"
[ "${PEDIDOS}" = 5 ] || falla "hay ${PEDIDOS} pedidos en la auditoría y se esperaban 5"
[ "${FINALES}" = 5 ] || falla "hay ${FINALES} finales en la auditoría y se esperaban 5"
[ "${ENROLADO}" = 1 ] || falla "el enrolamiento del agent no quedó en la auditoría"
verde "ok: ${PEDIDOS} pedidos y ${FINALES} finales en audit_log, y el enrolamiento del agent"

# --- El agent, después de todo eso ---

MEMORIA="$(systemctl show -p MemoryCurrent --value statussv-agent)"
verde "ok: el agent ocupa $((MEMORIA / 1024 / 1024)) MiB después de cinco deploys"

paso "listo"
verde "el deploy de systemd de punta a punta anduvo"
