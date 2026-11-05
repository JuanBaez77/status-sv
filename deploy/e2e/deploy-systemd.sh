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
#   6. El primer deploy de un proyecto que todavía no corre (ADR-021): el agent
#      clona en un workdir sin clonar, los pasos corren con el usuario del
#      proyecto y, si el servicio no queda bien, el ayudante para la unidad.
#   7. Todo quedó en audit_log.
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
# El proyecto que todavía no corre: se levanta con un primer deploy.
NUEVO="turnos-systemd"
UNIDAD_NUEVA="e2e-turnos.service"
PUERTO_NUEVO="${PUERTO_NUEVO:-18094}"
ORIGEN_NUEVO=/srv/e2e-systemd-nuevo-origen.git
WD_NUEVO=/srv/e2e-systemd-nuevo
DROPIN_DIR=/etc/systemd/system/statussv-agent.service.d

TRABAJO="$(mktemp -d)"
FUENTE="${TRABAJO}/fuente"
FUENTE_NUEVO="${TRABAJO}/fuente-nuevo"
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
    journalctl -u "${UNIDAD_NUEVA}" -n 15 --no-pager -o cat 2>/dev/null
  fi
  [ -n "${SERVER_PID}" ] && kill "${SERVER_PID}" 2>/dev/null
  wait 2>/dev/null
  systemctl disable --now statussv-agent statussv-unit-helper.socket "${UNIDAD}" "${UNIDAD_NUEVA}" >/dev/null 2>&1
  systemctl stop 'statussv-unit-helper@*.service' 'statussv-step-*.service' 2>/dev/null
  systemctl reset-failed statussv-agent "${UNIDAD}" "${UNIDAD_NUEVA}" 'statussv-unit-helper@*.service' 'statussv-step-*.service' 2>/dev/null
  rm -rf /etc/systemd/system/statussv-agent.service "${DROPIN_DIR}" "/etc/systemd/system/${UNIDAD}" \
    "/etc/systemd/system/${UNIDAD_NUEVA}" \
    /etc/systemd/system/statussv-unit-helper.socket /etc/systemd/system/statussv-unit-helper@.service
  systemctl daemon-reload
  rm -rf "${WD}" "${ORIGEN}" "${WD_NUEVO}" "${ORIGEN_NUEVO}" \
    /etc/status-sv /var/lib/status-sv /run/status-sv /usr/local/bin/statussv-agent
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
printf '[safe]\n\tdirectory = %s\n\tdirectory = %s\n' "${ORIGEN}" "${ORIGEN_NUEVO}" >"${TRABAJO}/gitconfig"
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

# --- El proyecto que todavía no corre ---
#
# Declarado y preparado por una persona, como dice el runbook, pero sin clonar
# y con su unidad instalada y parada. El workdir es del usuario del proyecto,
# que escribe ahí lo que prepara, y del grupo del agent, que trae el código.
"${G[@]}" init -q --bare "${ORIGEN_NUEVO}"
"${G[@]}" init -q "${FUENTE_NUEVO}"
"${G[@]}" -C "${FUENTE_NUEVO}" remote add origin "${ORIGEN_NUEVO}"
sed "s/${PUERTO_WEB}/${PUERTO_NUEVO}/" "${FUENTE}/app.py" >"${FUENTE_NUEVO}/app.py"
cat >"${FUENTE_NUEVO}/build.sh" <<'EOF'
#!/bin/sh
echo "preparando $(cat version) como $(id -un)"
mkdir -p var
echo "$(cat version) $(id -un)" > var/construido.txt
EOF
chmod 0755 "${FUENTE_NUEVO}/build.sh"
# version_nuevo commitea una versión del proyecto nuevo; con "roto", el
# servicio se cae al arrancar.
version_nuevo() {
  local nombre="$1" marca="${2:-}"
  rm -f "${FUENTE_NUEVO}/roto"
  echo "${nombre}" >"${FUENTE_NUEVO}/version"
  [ -n "${marca}" ] && echo si >"${FUENTE_NUEVO}/${marca}"
  "${G[@]}" -C "${FUENTE_NUEVO}" add -A
  "${G[@]}" -C "${FUENTE_NUEVO}" commit -qm "${nombre}"
  "${G[@]}" -C "${FUENTE_NUEVO}" push -q origin main
  "${G[@]}" -C "${ORIGEN_NUEVO}" rev-parse refs/heads/main
}
N1="$(version_nuevo n1 roto)"
chown -R statussv "${ORIGEN_NUEVO}"
install -d -o "${APP_USER}" -g statussv -m 2775 "${WD_NUEVO}"

cat >"/etc/systemd/system/${UNIDAD_NUEVA}" <<EOF
[Unit]
Description=Proyecto de la prueba de primer deploy de systemd

[Service]
User=${APP_USER}
WorkingDirectory=${WD_NUEVO}
ExecStart=/usr/bin/python3 ${WD_NUEVO}/app.py
Restart=always
RestartSec=1
EOF
systemctl daemon-reload

cat >"/etc/status-sv/projects.d/${NUEVO}.yaml" <<EOF
name: ${NUEVO}
environment: staging
repo: file://${ORIGEN_NUEVO}
branch: main
runtime: systemd
workdir: ${WD_NUEVO}
units:
  - ${UNIDAD_NUEVA}
run_as: ${APP_USER}
healthcheck:
  url: http://127.0.0.1:${PUERTO_NUEVO}/health
  expect_status: 200
  timeout_seconds: 2
deploy:
  build:
    - command: ["./build.sh"]
      timeout_seconds: 120
rollback: previous_commit
EOF
chown root:statussv "/etc/status-sv/projects.d/${NUEVO}.yaml"
chmod 0640 "/etc/status-sv/projects.d/${NUEVO}.yaml"

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
# El proyecto sin clonar no está roto: deploy-check dice que le toca un primer
# deploy, después de probar la escritura en el workdir y que el repo conteste.
grep -q "${NUEVO} todavía no se desplegó: está listo para su primer deploy" <<<"${INSTALADOR}" || {
  printf '%s\n' "${INSTALADOR}" | sed -n '/revisando el deploy/,$p'
  falla "deploy-check no reconoció el proyecto que espera su primer deploy"
}
[ ! -e "${WD_NUEVO}/.git" ] || falla "deploy-check dejó un repositorio en el workdir del proyecto nuevo"
verde "ok: deploy-check dice que el proyecto nuevo está listo para su primer deploy"

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
esperar "el agent declaró el proyecto" 90 declarado
ID_PROYECTO="$(proyecto_id)"

# pedir deja la respuesta en ${TRABAJO}/respuesta.json y devuelve el código
# HTTP. El tercer argumento es el proyecto; sin él, el de muestra.
pedir() {
  local tipo="$1" cuerpo="$2" proyecto="${3:-${ID_PROYECTO}}"
  api -o "${TRABAJO}/respuesta.json" -w '%{http_code}' --data "${cuerpo}" "${URL}/v1/projects/${proyecto}/${tipo}"
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

# --- 6. Primer deploy ---

paso "6. primer deploy de un proyecto que todavía no corre"
esperar "el agent declaró el proyecto nuevo" 90 declarado "${NUEVO}"
ID_NUEVO="$(proyecto_id "${NUEVO}")"
estado_unidad() { systemctl show -p ActiveState --value "${UNIDAD_NUEVA}"; }
sirve_nuevo() { [ "$(curl -fsS "http://127.0.0.1:${PUERTO_NUEVO}/$1" 2>/dev/null)" = "$2" ]; }
en_nuevo() { git -c "safe.directory=${WD_NUEVO}" -C "${WD_NUEVO}" rev-parse HEAD; }
[ "$(estado_unidad)" = inactive ] || falla "la unidad del proyecto nuevo ya estaba $(estado_unidad)"

# El primer intento no queda bien (n1 se cae al arrancar): el ayudante para
# la unidad, y el código clonado queda.
codigo_nuevo
codigo="$(pedir bootstrap "{\"confirm\":\"${NUEVO}\",\"totp_code\":\"${CODIGO}\"}" "${ID_NUEVO}")"
[ "${codigo}" = 201 ] || falla "el primer deploy devolvió ${codigo}: $(cat "${TRABAJO}/respuesta.json")"
INTENTO="$(jq -r .id "${TRABAJO}/respuesta.json")"
esperar "terminó el primer deploy que no queda bien" 300 termino "${INTENTO}"
DETALLE="$(detalle "${INTENTO}")"
[ "$(jq -r '.kind + " " + .status + " " + .code' <<<"${DETALLE}")" = "bootstrap rolled_back unhealthy" ] || falla "el primer intento terminó $(resumen "${DETALLE}")"
[ "$(pasos "${DETALLE}")" = "preflight:ok fetch:ok checkout:ok build:ok restart:ok healthcheck:failed rollback:ok" ] || falla "pasos: $(pasos "${DETALLE}")"
case "$(estado_unidad)" in
  inactive | failed) ;;
  *) falla "la unidad quedó $(estado_unidad) después de un primer deploy que no quedó bien" ;;
esac
[ "$(en_nuevo)" = "${N1}" ] || falla "el código clonado no quedó en el workdir"
verde "ok: no quedó bien, el ayudante paró la unidad y el código quedó clonado"

# Con una versión que anda, queda.
N2="$(version_nuevo n2)"
codigo_nuevo
codigo="$(pedir bootstrap "{\"confirm\":\"${NUEVO}\",\"totp_code\":\"${CODIGO}\"}" "${ID_NUEVO}")"
[ "${codigo}" = 201 ] || falla "el segundo primer deploy devolvió ${codigo}: $(cat "${TRABAJO}/respuesta.json")"
PRIMERO="$(jq -r .id "${TRABAJO}/respuesta.json")"
esperar "terminó el primer deploy" 180 termino "${PRIMERO}"
DETALLE="$(detalle "${PRIMERO}")"
[ "$(jq -r '.kind + " " + .status' <<<"${DETALLE}")" = "bootstrap succeeded" ] || falla "el primer deploy terminó $(resumen "${DETALLE}")"
[ "$(jq -r .commit <<<"${DETALLE}")" = "${N2}" ] || falla "el primer deploy no es de la punta de la rama"
[ "$(pasos "${DETALLE}")" = "preflight:ok fetch:ok checkout:ok build:ok restart:ok healthcheck:ok" ] || falla "pasos: $(pasos "${DETALLE}")"
[ "$(estado_unidad)" = active ] || falla "la unidad quedó $(estado_unidad)"
sirve_nuevo version n2 || falla "el proyecto nuevo no sirve n2"
# El código lo trajo el agent; lo que preparó el paso build es del usuario del
# proyecto.
sirve_nuevo construido "n2 ${APP_USER}" || falla "el paso build no corrió con el usuario del proyecto"
[ "$(stat -c %U "${WD_NUEVO}/.git")" = statussv ] || falla "el repositorio no es del agent"
[ "$(stat -c %U "${WD_NUEVO}/var/construido.txt")" = "${APP_USER}" ] || falla "lo que preparó el paso no es del usuario del proyecto"
verde "ok: el agent clonó, el paso build corrió como ${APP_USER} y el servicio quedó andando en n2"

# --- 7. Auditoría ---

paso "7. auditoría"
contar() { psql "${DATABASE_URL}" -tAc "$1" | tr -d '[:space:]'; }
PEDIDOS="$(contar "SELECT count(*) FROM audit_log WHERE action IN ('deploy.request','rollback.request') AND actor = '${USUARIO}' AND result = 'accepted'")"
PRIMEROS="$(contar "SELECT count(*) FROM audit_log WHERE action = 'bootstrap.request' AND actor = '${USUARIO}' AND result = 'accepted'")"
FINALES="$(contar "SELECT count(*) FROM audit_log WHERE action = 'deploy.finish' AND actor = '${SERVER_NAME}' AND actor_type = 'agent'")"
ENROLADO="$(contar "SELECT count(*) FROM audit_log WHERE action = 'agent.enroll' AND actor = '${SERVER_NAME}' AND result = 'ok'")"
[ "${PEDIDOS}" = 5 ] || falla "hay ${PEDIDOS} pedidos en la auditoría y se esperaban 5"
[ "${PRIMEROS}" = 2 ] || falla "hay ${PRIMEROS} pedidos de primer deploy en la auditoría y se esperaban 2"
[ "${FINALES}" = 7 ] || falla "hay ${FINALES} finales en la auditoría y se esperaban 7"
[ "${ENROLADO}" = 1 ] || falla "el enrolamiento del agent no quedó en la auditoría"
verde "ok: ${PEDIDOS} pedidos, ${PRIMEROS} primeros deploys y ${FINALES} finales en audit_log, y el enrolamiento del agent"

# --- El agent, después de todo eso ---

MEMORIA="$(systemctl show -p MemoryCurrent --value statussv-agent)"
verde "ok: el agent ocupa $((MEMORIA / 1024 / 1024)) MiB después de siete deploys"

# --- 8. Apagar el deploy ---

# Con el binario ya instalado, como dice el runbook: el instalador no puede
# copiarlo sobre sí mismo.
paso "8. --disable-deploy saca el drop-in, el ayudante y su socket"
if ! APAGADO="$(bash "${RAIZ}/deploy/install-agent.sh" --disable-deploy /usr/local/bin/statussv-agent 2>&1)"; then
  printf '%s\n' "${APAGADO}"
  falla "el instalador falló al apagar el deploy"
fi
[ ! -e "${DROPIN_DIR}/20-deploy.conf" ] || falla "--disable-deploy no sacó el drop-in"
! systemctl is-active --quiet statussv-unit-helper.socket || falla "el ayudante sigue activo"
[ ! -e /etc/systemd/system/statussv-unit-helper.socket ] || falla "--disable-deploy no sacó la unidad del ayudante"
[ ! -e /run/status-sv/unit-helper.sock ] || falla "el socket del ayudante quedó en /run"
systemctl is-active --quiet statussv-agent || falla "el agent no quedó andando con el deploy apagado"
verde "ok: deploy apagado, sin ayudante ni socket, y el agent andando"

paso "listo"
verde "el deploy de systemd de punta a punta anduvo"
