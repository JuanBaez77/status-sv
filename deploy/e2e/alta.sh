#!/usr/bin/env bash
#
# El alta de un proyecto de punta a punta (ADR-022 y ADR-023): el control
# plane, PostgreSQL, un agent instalado de verdad con install-agent.sh, nginx y
# Docker, en una máquina sin ningún proyecto declarado.
#
#   1. Un proyecto de systemd se propone por la API, como lo hace el panel, y
#      le llega al agent.
#   2. Se acepta en el servidor con `project accept --yes --env-file`: crea el
#      usuario, el workdir, la deploy key, el archivo de entorno, la unidad y el
#      sitio de nginx con su certificado. Como es el primer proyecto de systemd
#      del servidor, avisa que falta el ayudante root, y se corre el instalador
#      de nuevo, como dice el aviso.
#   3. El control plane se entera de la aceptación, y el panel ve las
#      variables cargadas, la llave pública y el sitio.
#   4. El primer deploy lo deja andando, habilitado al arrancar, y respondiendo
#      por HTTPS a través de nginx con el valor de su variable.
#   5. Lo mismo con un proyecto de Docker, con el .env en el workdir.
#   6. Los valores de las variables no están en la base, en los registros del
#      control plane ni en los del agent.
#   7. Todo quedó en audit_log.
#
# Let's Encrypt no puede firmar un dominio de prueba: un certbot de mentira
# hace lo mismo que el de verdad con --webroot (deja un desafío en el
# directorio, lo pide por HTTP a través de nginx) y firma un certificado él
# mismo. Los dominios de la prueba apuntan a 127.0.0.1 en /etc/hosts.
#
# Se corre como root en una máquina descartable con systemd como PID 1: el
# runner del CI. Instala y saca lo suyo. Hace falta:
#
#   DATABASE_URL   una base vacía de PostgreSQL
#   SERVER_BIN     el binario del control plane para Linux
#   AGENT_BIN      el binario del agent para Linux
#
#   sudo DATABASE_URL=postgres://... SERVER_BIN=... AGENT_BIN=... ./deploy/e2e/alta.sh
#
set -euo pipefail
shopt -s inherit_errexit

cd "$(dirname "$0")/../.."
RAIZ="$(pwd)"

: "${DATABASE_URL:?hace falta DATABASE_URL, una base vacía para la prueba}"
: "${SERVER_BIN:?hace falta SERVER_BIN, el binario del control plane}"
: "${AGENT_BIN:?hace falta AGENT_BIN, el binario del agent}"
export DATABASE_URL

PUERTO="${PUERTO:-8098}"
URL="http://127.0.0.1:${PUERTO}"
SERVER_NAME="vps-alta-e2e"
USUARIO="alta"
CLAVE="clave-de-la-prueba-del-alta"
CORREO="e2e@status-sv.invalid"

SISTEMA="svdemo-e2e"
PUERTO_SISTEMA="${PUERTO_SISTEMA:-18101}"
DOMINIO_SISTEMA="svdemo.alta-e2e.test"
ORIGEN_SISTEMA=/srv/e2e-alta-sistema.git

DOCKER="tienda-alta"
PUERTO_DOCKER="${PUERTO_DOCKER:-18102}"
DOMINIO_DOCKER="tienda.alta-e2e.test"
ORIGEN_DOCKER=/srv/e2e-alta-docker.git

# Los valores de las variables: al azar, para buscarlos después donde no
# tienen que estar.
SECRETO_SISTEMA="valor-systemd-$(openssl rand -hex 12)"
SECRETO_DOCKER="valor-docker-$(openssl rand -hex 12)"

DROPIN_DIR=/etc/systemd/system/statussv-agent.service.d
CERTBOT=/usr/local/bin/certbot
HOSTS_MARCA="# statussv: prueba del alta"
ACME=/var/www/statussv-acme

TRABAJO="$(mktemp -d)"
SERVER_PID=""
MARCA_USUARIO="${TRABAJO}/creo-statussv"
MARCA_ACME="${TRABAJO}/creo-acme"

rojo() { printf '\033[31m%s\033[0m\n' "$*" >&2; }
verde() { printf '\033[32m%s\033[0m\n' "$*"; }
paso() { printf '\n=== %s ===\n' "$*"; }

[ "$(id -u)" -eq 0 ] || { rojo "hay que correrlo como root"; exit 2; }
for p in systemctl git curl jq openssl base32 python3 psql nginx docker ssh-keygen sha256sum; do
  command -v "$p" >/dev/null || { rojo "falta ${p}"; exit 2; }
done
[ -x "${SERVER_BIN}" ] && [ -x "${AGENT_BIN}" ] || { rojo "SERVER_BIN y AGENT_BIN tienen que ser binarios para Linux"; exit 2; }
if [ -e /etc/systemd/system/statussv-agent.service ] || [ -e /etc/status-sv ]; then
  rojo "esta máquina ya tiene un agent de status-sv instalado: la prueba lo pisaría"
  exit 2
fi
if command -v certbot >/dev/null || [ -e /etc/letsencrypt ]; then
  rojo "esta máquina tiene certbot o certificados: la prueba pone uno de mentira y no los quiere pisar"
  exit 2
fi
[ -d /etc/nginx/sites-available ] && [ -d /etc/nginx/sites-enabled ] || { rojo "este nginx no usa sites-available y sites-enabled"; exit 2; }

compose_docker() { docker compose --project-directory "/srv/${DOCKER}" -f "/srv/${DOCKER}/compose.yml" -p "${DOCKER}" "$@"; }

limpiar() {
  local salida=$?
  set +e
  if [ $salida -ne 0 ]; then
    rojo "la prueba falló; últimos registros:"
    tail -n 30 "${TRABAJO}/server.log" 2>/dev/null
    journalctl -u statussv-agent -n 40 --no-pager -o cat 2>/dev/null
    journalctl -u 'statussv-unit-helper@*' -n 20 --no-pager -o cat 2>/dev/null
    journalctl -u "${SISTEMA}.service" -n 15 --no-pager -o cat 2>/dev/null
    tail -n 20 /var/log/nginx/error.log 2>/dev/null
  fi
  [ -n "${SERVER_PID}" ] && kill "${SERVER_PID}" 2>/dev/null
  wait 2>/dev/null
  [ -f "/srv/${DOCKER}/compose.yml" ] && compose_docker down --rmi local --volumes >/dev/null 2>&1
  systemctl disable --now statussv-agent statussv-unit-helper.socket "${SISTEMA}.service" >/dev/null 2>&1
  systemctl stop 'statussv-unit-helper@*.service' 'statussv-step-*.service' 2>/dev/null
  systemctl reset-failed statussv-agent "${SISTEMA}.service" 'statussv-unit-helper@*.service' 'statussv-step-*.service' 2>/dev/null
  rm -rf /etc/systemd/system/statussv-agent.service "${DROPIN_DIR}" "/etc/systemd/system/${SISTEMA}.service" \
    /etc/systemd/system/statussv-unit-helper.socket /etc/systemd/system/statussv-unit-helper@.service
  systemctl daemon-reload
  rm -f /etc/nginx/sites-enabled/statussv-*.conf /etc/nginx/sites-available/statussv-*.conf
  nginx -t >/dev/null 2>&1 && systemctl reload nginx
  rm -rf /etc/letsencrypt "${CERTBOT}"
  [ -f "${MARCA_ACME}" ] && rm -rf "${ACME}"
  sed -i "/${HOSTS_MARCA}/d" /etc/hosts
  rm -rf "/srv/${SISTEMA}" "/srv/${DOCKER}" "${ORIGEN_SISTEMA}" "${ORIGEN_DOCKER}" \
    /etc/status-sv /var/lib/status-sv /run/status-sv /usr/local/bin/statussv-agent
  userdel "${SISTEMA}" 2>/dev/null
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

hash_de() { printf '%s' "$1" | sha256sum | cut -d' ' -f1; }

# --- Lo que hace falta en el servidor ---

paso "preparando el servidor: nginx, los dominios y un certbot de mentira"
systemctl is-active --quiet nginx || systemctl start nginx
printf '127.0.0.1 %s %s %s\n' "${DOMINIO_SISTEMA}" "${DOMINIO_DOCKER}" "${HOSTS_MARCA}" >>/etc/hosts
[ -d "${ACME}" ] || touch "${MARCA_ACME}"

# El certbot de mentira: con --webroot, deja el desafío en el directorio y lo
# pide por HTTP al dominio, a través de nginx, como Let's Encrypt. Si llega,
# firma él mismo el certificado donde lo deja el de verdad. Con --dry-run solo
# prueba el desafío.
cat >"${CERTBOT}" <<'EOF'
#!/bin/sh
set -eu
nombre="" dominio="" webroot="" prueba=no
while [ $# -gt 0 ]; do
  case "$1" in
    --cert-name) nombre="$2"; shift ;;
    -d) dominio="$2"; shift ;;
    -w) webroot="$2"; shift ;;
    --dry-run) prueba=si ;;
  esac
  shift
done
[ -n "${nombre}" ] && [ -n "${dominio}" ] && [ -n "${webroot}" ] || { echo "certbot de prueba: faltan --cert-name, -d o -w" >&2; exit 2; }
token="prueba-$$"
mkdir -p "${webroot}/.well-known/acme-challenge"
echo "${token}" >"${webroot}/.well-known/acme-challenge/${token}"
# systemctl reload nginx vuelve antes de que se vayan los workers viejos, que
# todavía pueden atender un pedido con la configuración anterior. El agent ya
# esperó a que uno nuevo sirviera el desafío, pero el pedido de acá puede caer
# en uno viejo: como Let's Encrypt, se le da margen, hasta 10 intentos cada
# medio segundo. Si falla, llego se queda con lo que dijo curl.
intento=1
while :; do
  llego="$(curl -fsS --max-time 2 "http://${dominio}/.well-known/acme-challenge/${token}" 2>&1)" && [ "${llego}" = "${token}" ] && break
  [ "${intento}" -ge 10 ] && break
  intento=$((intento + 1))
  sleep 0.5
done
rm -f "${webroot}/.well-known/acme-challenge/${token}"
[ "${llego}" = "${token}" ] || { echo "certbot de prueba: el desafío no llegó por http://${dominio} en ${intento} intentos: ${llego}" >&2; exit 1; }
[ "${prueba}" = si ] && { echo "certbot de prueba: el desafío llegó (dry run)"; exit 0; }
mkdir -p "/etc/letsencrypt/live/${nombre}"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=${dominio}" -addext "subjectAltName=DNS:${dominio}" \
  -keyout "/etc/letsencrypt/live/${nombre}/privkey.pem" -out "/etc/letsencrypt/live/${nombre}/fullchain.pem" 2>/dev/null
echo "certbot de prueba: certificado para ${dominio}"
EOF
chmod 0755 "${CERTBOT}"

# --- Los repositorios de los proyectos ---

paso "armando los repositorios de los dos proyectos"
if ! id statussv >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin statussv
  touch "${MARCA_USUARIO}"
fi
G=(git -c user.name=e2e -c user.email=e2e@status-sv.invalid -c init.defaultBranch=main)

# El de systemd: un servidor HTTP mínimo, como haría gunicorn. /secreto
# devuelve el hash de su variable: así se ve que llegó sin mostrarla.
FUENTE="${TRABAJO}/fuente-sistema"
"${G[@]}" init -q --bare "${ORIGEN_SISTEMA}"
"${G[@]}" init -q "${FUENTE}"
cat >"${FUENTE}/app.py" <<EOF
import hashlib, http.server, os

def leer(ruta):
    try:
        return open(ruta).read()
    except OSError:
        return ""

class Sitio(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        secreto = hashlib.sha256(os.environ.get("SECRET_KEY", "").encode()).hexdigest()
        cuerpo = {"/health": "ok", "/secreto": secreto, "/construido": leer("var/construido.txt")}.get(self.path)
        self.send_response(200 if cuerpo is not None else 404)
        self.end_headers()
        self.wfile.write((cuerpo or "").encode())

    def log_message(self, *args):
        pass

http.server.HTTPServer(("127.0.0.1", ${PUERTO_SISTEMA}), Sitio).serve_forever()
EOF
cat >"${FUENTE}/build.sh" <<'EOF'
#!/bin/sh
echo "preparando como $(id -un)"
mkdir -p var
id -un > var/construido.txt
EOF
chmod 0755 "${FUENTE}/build.sh"
"${G[@]}" -C "${FUENTE}" add -A
"${G[@]}" -C "${FUENTE}" commit -qm v1
"${G[@]}" -C "${FUENTE}" push -q "${ORIGEN_SISTEMA}" main

# El de Docker: busybox sirve el hash de su variable, que le pasa Compose
# desde el .env del workdir.
FUENTE="${TRABAJO}/fuente-docker"
"${G[@]}" init -q --bare "${ORIGEN_DOCKER}"
"${G[@]}" init -q "${FUENTE}"
printf 'FROM %s/busybox:1.37\n' "${IMAGENES:-docker.io/library}" >"${FUENTE}/Dockerfile"
cat >>"${FUENTE}/Dockerfile" <<'EOF'
CMD ["sh", "-c", "mkdir -p /www && echo ok > /www/health && printf %s \"$SECRET_KEY\" | sha256sum | cut -d' ' -f1 > /www/secreto && exec httpd -f -p 8080 -h /www"]
EOF
cat >"${FUENTE}/compose.yml" <<EOF
services:
  web:
    build: .
    environment:
      SECRET_KEY: \${SECRET_KEY:?falta el .env del workdir}
    ports:
      - "127.0.0.1:${PUERTO_DOCKER}:8080"
EOF
"${G[@]}" -C "${FUENTE}" add -A
"${G[@]}" -C "${FUENTE}" commit -qm v1
"${G[@]}" -C "${FUENTE}" push -q "${ORIGEN_DOCKER}" main
# git no sirve por file:// un repo de otro dueño: los orígenes son del agent.
chown -R statussv "${ORIGEN_SISTEMA}" "${ORIGEN_DOCKER}"

# --- Control plane ---

paso "levantando el control plane"
openssl rand -base64 32 >"${TRABAJO}/totp.key"
chmod 600 "${TRABAJO}/totp.key"
export STATUSSV_TOTP_KEY_FILE="${TRABAJO}/totp.key"
export STATUSSV_LISTEN_ADDR="127.0.0.1:${PUERTO}"
export STATUSSV_LOG_FORMAT=text
export STATUSSV_LOG_LEVEL=debug
"${SERVER_BIN}" migrate up >/dev/null
"${SERVER_BIN}" serve >"${TRABAJO}/server.log" 2>&1 &
SERVER_PID=$!
esperar "el control plane responde /readyz" 60 curl -fsS "${URL}/readyz"

printf '%s\n' "${CLAVE}" | "${SERVER_BIN}" user-add --username "${USUARIO}" >/dev/null
TOKEN="$("${SERVER_BIN}" enroll-token --server "${SERVER_NAME}" | grep -o 'svenr_[A-Za-z0-9_-]*' | head -1)"
[ -n "${TOKEN}" ] || falla "el control plane no devolvió ningún token"

# --- El agent, sin ningún proyecto ---

paso "instalando el agent con --enable-deploy, sin proyectos"
# Lo único que no es de un servidor de verdad: aceptar un repo file://.
install -d -m 0755 "${DROPIN_DIR}"
printf '[Service]\nEnvironment=STATUSSV_DEPLOY_ALLOW_FILE_REPOS=true\nEnvironment=STATUSSV_LOG_LEVEL=debug\n' >"${DROPIN_DIR}/30-e2e.conf"
export STATUSSV_DEPLOY_ALLOW_FILE_REPOS=true
instalar() {
  if ! INSTALADOR="$(STATUSSV_SERVER_URL="${URL}" STATUSSV_ENROLL_TOKEN="${TOKEN}" \
    bash "${RAIZ}/deploy/install-agent.sh" --enable-deploy "${AGENT_BIN}" 2>&1)"; then
    printf '%s\n' "${INSTALADOR}"
    falla "el instalador falló"
  fi
}
instalar
grep -q "ninguno todavía" <<<"${INSTALADOR}" || falla "el instalador encontró proyectos en un servidor vacío"
verde "ok: agent instalado, sin proyectos"

# --- Sesión del panel, con el segundo factor ---

paso "iniciando sesión en el panel"
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

codigo_nuevo() {
  while [ $(($(date +%s) / 30)) -le "${PASO_USADO}" ]; do sleep 1; done
  PASO_USADO=$(($(date +%s) / 30))
  CODIGO="$(totp_code "${SECRETO}" "${PASO_USADO}")"
}

api() { curl -sS -H "Cookie: ${SESION}" -H 'Content-Type: application/json' "$@"; }

servidor_id() { api "${URL}/v1/servers" | jq -r ".servers[] | select(.name == \"${SERVER_NAME}\") | .id"; }
SERVIDOR="$(servidor_id)"
[ -n "${SERVIDOR}" ] || falla "el servidor no aparece en el panel"

estado_propuesta() { api "${URL}/v1/proposals/$1" | jq -r .status; }
propuesta_en() { [ "$(estado_propuesta "$1")" = "$2" ]; }
proyecto_id() { api "${URL}/v1/projects" | jq -r ".projects[] | select(.name == \"$1\") | .id"; }
proyecto() { api "${URL}/v1/projects/$(proyecto_id "$1")"; }
declarado() { [ -n "$(proyecto_id "$1")" ]; }
estado() { api "${URL}/v1/deploys/$1" | jq -r .status; }
termino() { case "$(estado "$1")" in succeeded | failed | rolled_back) return 0 ;; *) return 1 ;; esac }
pasos() { jq -r '[.steps[] | .name + ":" + .status] | join(" ")' <<<"$1"; }
resumen() { jq -c '{status,code,message}' <<<"$1"; }

# proponer manda una propuesta y deja su ID en PROPUESTA.
proponer() {
  local codigo
  codigo="$(api -o "${TRABAJO}/respuesta.json" -w '%{http_code}' --data "$1" "${URL}/v1/servers/${SERVIDOR}/proposals")"
  [ "${codigo}" = 201 ] || falla "la propuesta devolvió ${codigo}: $(cat "${TRABAJO}/respuesta.json")"
  PROPUESTA="$(jq -r .id "${TRABAJO}/respuesta.json")"
}

# aceptar corre el comando de aceptación como lo correría una persona, con
# los valores en un archivo.
aceptar() {
  local nombre="$1" valor="$2"
  install -m 0600 /dev/null "${TRABAJO}/${nombre}.env"
  printf 'SECRET_KEY=%s\n' "${valor}" >"${TRABAJO}/${nombre}.env"
  if ! statussv-agent project accept --yes --env-file "${TRABAJO}/${nombre}.env" --email "${CORREO}" "${nombre}" \
    >"${TRABAJO}/aceptar-${nombre}.txt" 2>&1; then
    cat "${TRABAJO}/aceptar-${nombre}.txt"
    falla "project accept ${nombre} falló"
  fi
  rm -f "${TRABAJO}/${nombre}.env"
  grep -qF "${valor}" "${TRABAJO}/aceptar-${nombre}.txt" && falla "project accept mostró el valor de la variable"
  return 0
}

# primer_deploy pide el primer deploy con un código nuevo y espera su final.
primer_deploy() {
  local nombre="$1" id codigo deploy detalle
  id="$(proyecto_id "${nombre}")"
  codigo_nuevo
  codigo="$(api -o "${TRABAJO}/respuesta.json" -w '%{http_code}' \
    --data "{\"confirm\":\"${nombre}\",\"totp_code\":\"${CODIGO}\"}" "${URL}/v1/projects/${id}/bootstrap")"
  [ "${codigo}" = 201 ] || falla "el primer deploy de ${nombre} devolvió ${codigo}: $(cat "${TRABAJO}/respuesta.json")"
  deploy="$(jq -r .id "${TRABAJO}/respuesta.json")"
  esperar "terminó el primer deploy de ${nombre}" 300 termino "${deploy}"
  detalle="$(api "${URL}/v1/deploys/${deploy}")"
  [ "$(jq -r '.kind + " " + .status' <<<"${detalle}")" = "bootstrap succeeded" ] \
    || falla "el primer deploy de ${nombre} terminó $(resumen "${detalle}"): $(pasos "${detalle}")"
  PASOS="$(pasos "${detalle}")"
}

# por_nginx pide una ruta del dominio por HTTPS, a través de nginx.
por_nginx() {
  local dominio="$1" ruta="$2"
  curl -fsS --cacert "/etc/letsencrypt/live/statussv-$3/fullchain.pem" \
    --resolve "${dominio}:443:127.0.0.1" "https://${dominio}${ruta}"
}

# --- 1. La propuesta de systemd ---

paso "1. se propone un proyecto de systemd"
proponer "$(jq -nc --arg repo "file://${ORIGEN_SISTEMA}" --arg dominio "${DOMINIO_SISTEMA}" \
  --argjson puerto "${PUERTO_SISTEMA}" --arg salud "http://127.0.0.1:${PUERTO_SISTEMA}/health" \
  '{name: "'"${SISTEMA}"'", environment: "staging", repo: $repo, branch: "main", runtime: "systemd",
    command: ["/usr/bin/python3", "app.py"], required_env: ["SECRET_KEY"],
    site: {domain: $dominio, port: $puerto}, healthcheck: {url: $salud},
    build: [{command: ["./build.sh"]}]}')"
PROPUESTA_SISTEMA="${PROPUESTA}"
esperar "la propuesta le llegó al agent" 60 propuesta_en "${PROPUESTA_SISTEMA}" delivered
[ -f "/var/lib/status-sv/proposals/${SISTEMA}.yaml" ] || falla "el agent no guardó la propuesta"
declarado "${SISTEMA}" && falla "una propuesta apareció como proyecto declarado"
verde "ok: el agent la guardó y nada cambió en el servidor"

# --- 2. La aceptación ---

paso "2. se acepta en el servidor"
aceptar "${SISTEMA}" "${SECRETO_SISTEMA}"
SALIDA="$(cat "${TRABAJO}/aceptar-${SISTEMA}.txt")"
grep -q "sitio listo: https://${DOMINIO_SISTEMA}" <<<"${SALIDA}" || { printf '%s\n' "${SALIDA}"; falla "el sitio no quedó"; }
grep -q "el ayudante root de systemd no está instalado" <<<"${SALIDA}" || { printf '%s\n' "${SALIDA}"; falla "no avisó que falta el ayudante"; }
id "${SISTEMA}" >/dev/null 2>&1 || falla "no se creó el usuario del proyecto"
[ "$(stat -c '%U:%G %a' "/srv/${SISTEMA}")" = "${SISTEMA}:statussv 2775" ] || falla "el workdir quedó $(stat -c '%U:%G %a' "/srv/${SISTEMA}")"
[ "$(stat -c '%U %a' "/etc/status-sv/env/${SISTEMA}.env")" = "root 600" ] || falla "el archivo de entorno quedó $(stat -c '%U %a' "/etc/status-sv/env/${SISTEMA}.env")"
grep -q "^SECRET_KEY='${SECRETO_SISTEMA}'$" "/etc/status-sv/env/${SISTEMA}.env" || falla "el archivo de entorno no tiene la variable"
[ -f "/etc/systemd/system/${SISTEMA}.service" ] || falla "no se escribió la unidad"
[ -f "/etc/status-sv/keys/${SISTEMA}" ] && [ -f "/etc/status-sv/keys/${SISTEMA}.pub" ] || falla "no se creó la deploy key"
[ -f "/etc/status-sv/projects.d/${SISTEMA}.yaml" ] || falla "no quedó declarado"
[ ! -e "/var/lib/status-sv/proposals/${SISTEMA}.yaml" ] || falla "la propuesta quedó guardada después de aceptarla"
[ -f "/etc/nginx/sites-enabled/statussv-${SISTEMA}.conf" ] || falla "el sitio no está habilitado"
nginx -t >/dev/null 2>&1 || falla "nginx quedó con una configuración que no pasa nginx -t"
verde "ok: usuario, workdir, archivo de entorno, unidad, declaración y sitio con su certificado"

# El aviso dice qué hacer: correr el instalador de nuevo, que deja el
# ayudante y conserva la credencial.
instalar
systemctl is-active --quiet statussv-unit-helper.socket || falla "el instalador no dejó el ayudante"
grep -q "${SISTEMA} todavía no se desplegó: está listo para su primer deploy" <<<"${INSTALADOR}" || {
  printf '%s\n' "${INSTALADOR}" | sed -n '/revisando el deploy/,$p'
  falla "deploy-check no aprobó el proyecto aceptado"
}
verde "ok: el instalador dejó el ayudante y deploy-check aprobó el proyecto"

# --- 3. El control plane se entera ---

paso "3. el control plane ve la aceptación"
esperar "la propuesta quedó aceptada" 90 propuesta_en "${PROPUESTA_SISTEMA}" accepted
[ "$(api "${URL}/v1/proposals/${PROPUESTA_SISTEMA}" | jq -r .message)" = "declarada igual a la propuesta" ] \
  || falla "la aceptación no coincide con la propuesta"
variables_cargadas() { proyecto "$1" | jq -e '.env.required == 1 and (.env.missing | length) == 0 and (.env.unknown | not)'; }
esperar "el panel ve la variable cargada" 90 variables_cargadas "${SISTEMA}"
DETALLE="$(proyecto "${SISTEMA}")"
[ "$(jq -r .site.domain <<<"${DETALLE}")" = "${DOMINIO_SISTEMA}" ] || falla "el panel no ve el sitio"
jq -r .deploy_key <<<"${DETALLE}" | grep -q '^ssh-ed25519 ' || falla "el panel no ve la llave pública"
verde "ok: aceptada, con la variable cargada, la llave pública y el sitio"

# --- 4. El primer deploy ---

paso "4. el primer deploy del proyecto de systemd"
primer_deploy "${SISTEMA}"
[ "${PASOS}" = "preflight:ok fetch:ok checkout:ok build:ok restart:ok healthcheck:ok" ] || falla "pasos: ${PASOS}"
[ "$(systemctl is-enabled "${SISTEMA}.service")" = enabled ] || falla "la unidad no quedó habilitada al arrancar"
[ "$(por_nginx "${DOMINIO_SISTEMA}" /health "${SISTEMA}")" = ok ] || falla "el sitio no contesta por HTTPS"
[ "$(por_nginx "${DOMINIO_SISTEMA}" /secreto "${SISTEMA}")" = "$(hash_de "${SECRETO_SISTEMA}")" ] || falla "el servicio no recibió su variable"
[ "$(por_nginx "${DOMINIO_SISTEMA}" /construido "${SISTEMA}")" = "${SISTEMA}" ] || falla "el paso build no corrió con el usuario del proyecto"
REDIRECCION="$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' --resolve "${DOMINIO_SISTEMA}:80:127.0.0.1" "http://${DOMINIO_SISTEMA}/health")"
[ "${REDIRECCION}" = "301 https://${DOMINIO_SISTEMA}/health" ] || falla "por HTTP contestó ${REDIRECCION}"
verde "ok: anda, queda al arrancar y contesta por HTTPS a través de nginx con su variable"

# --- 5. Un proyecto de Docker ---

paso "5. el alta de un proyecto de Docker"
proponer "$(jq -nc --arg repo "file://${ORIGEN_DOCKER}" --arg dominio "${DOMINIO_DOCKER}" \
  --argjson puerto "${PUERTO_DOCKER}" --arg salud "http://127.0.0.1:${PUERTO_DOCKER}/health" \
  '{name: "'"${DOCKER}"'", environment: "staging", repo: $repo, branch: "main", runtime: "docker",
    compose_file: "compose.yml", required_env: ["SECRET_KEY"],
    site: {domain: $dominio, port: $puerto}, healthcheck: {url: $salud}}')"
PROPUESTA_DOCKER="${PROPUESTA}"
esperar "la propuesta de Docker le llegó al agent" 60 propuesta_en "${PROPUESTA_DOCKER}" delivered
aceptar "${DOCKER}" "${SECRETO_DOCKER}"
SALIDA="$(cat "${TRABAJO}/aceptar-${DOCKER}.txt")"
grep -q "sitio listo: https://${DOMINIO_DOCKER}" <<<"${SALIDA}" || { printf '%s\n' "${SALIDA}"; falla "el sitio de Docker no quedó"; }
[ "$(stat -c '%U:%G %a' "/srv/${DOCKER}/.env")" = "root:statussv 640" ] || falla "el .env quedó $(stat -c '%U:%G %a' "/srv/${DOCKER}/.env")"
esperar "la propuesta de Docker quedó aceptada" 90 propuesta_en "${PROPUESTA_DOCKER}" accepted
esperar "el panel ve la variable de Docker cargada" 90 variables_cargadas "${DOCKER}"
primer_deploy "${DOCKER}"
[ "$(por_nginx "${DOMINIO_DOCKER}" /health "${DOCKER}")" = ok ] || falla "el sitio de Docker no contesta por HTTPS"
[ "$(por_nginx "${DOMINIO_DOCKER}" /secreto "${DOCKER}")" = "$(hash_de "${SECRETO_DOCKER}")" ] || falla "el contenedor no recibió su variable"
verde "ok: propuesto, aceptado, desplegado y contestando por HTTPS con su variable"

# --- 6. Los valores no salieron del servidor ---

paso "6. los valores de las variables no están en el control plane"
: >"${TRABAJO}/base.txt"
for tabla in $(psql "${DATABASE_URL}" -tAc "SELECT tablename FROM pg_tables WHERE schemaname = 'public'"); do
  psql "${DATABASE_URL}" -qc "COPY (SELECT * FROM ${tabla}) TO STDOUT" >>"${TRABAJO}/base.txt"
done
[ -s "${TRABAJO}/base.txt" ] || falla "no se pudo leer la base"
journalctl -u statussv-agent --no-pager -o cat >"${TRABAJO}/agent.log" 2>/dev/null
for valor in "${SECRETO_SISTEMA}" "${SECRETO_DOCKER}"; do
  grep -qF "${valor}" "${TRABAJO}/base.txt" && falla "el valor de una variable está en la base"
  grep -qF "${valor}" "${TRABAJO}/server.log" && falla "el valor de una variable está en los registros del control plane"
  grep -qF "${valor}" "${TRABAJO}/agent.log" && falla "el valor de una variable está en los registros del agent"
done
verde "ok: ni en la base ($(wc -l <"${TRABAJO}/base.txt") renglones), ni en los registros del control plane, ni en los del agent"

# --- 7. Auditoría ---

paso "7. auditoría"
contar() { psql "${DATABASE_URL}" -tAc "$1" | tr -d '[:space:]'; }
PROPUESTAS="$(contar "SELECT count(*) FROM audit_log WHERE action = 'project.propose' AND actor = '${USUARIO}' AND actor_type = 'user' AND result = 'ok'")"
ACEPTADAS="$(contar "SELECT count(*) FROM audit_log WHERE action = 'project.accept' AND actor = '${SERVER_NAME}' AND actor_type = 'agent'")"
PRIMEROS="$(contar "SELECT count(*) FROM audit_log WHERE action = 'bootstrap.request' AND actor = '${USUARIO}' AND result = 'accepted'")"
FINALES="$(contar "SELECT count(*) FROM audit_log WHERE action = 'deploy.finish' AND actor = '${SERVER_NAME}' AND result = 'succeeded'")"
[ "${PROPUESTAS}" = 2 ] || falla "hay ${PROPUESTAS} propuestas en la auditoría y se esperaban 2"
[ "${ACEPTADAS}" = 2 ] || falla "hay ${ACEPTADAS} aceptaciones en la auditoría y se esperaban 2"
[ "${PRIMEROS}" = 2 ] || falla "hay ${PRIMEROS} pedidos de primer deploy en la auditoría y se esperaban 2"
[ "${FINALES}" = 2 ] || falla "hay ${FINALES} finales en la auditoría y se esperaban 2"
verde "ok: las propuestas, las aceptaciones y los primeros deploys en audit_log"

MEMORIA="$(systemctl show -p MemoryCurrent --value statussv-agent)"
verde "ok: el agent ocupa $((MEMORIA / 1024 / 1024)) MiB después de dos altas"

paso "listo"
verde "el alta de punta a punta anduvo"
