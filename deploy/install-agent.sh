#!/usr/bin/env bash
#
# Instala o actualiza el agent de status-sv como servicio de systemd.
#
# Se corre en el VPS, como root, con el binario ya copiado al servidor:
#
#   STATUSSV_SERVER_URL=https://status.ejemplo.com ./install-agent.sh ./statussv-agent
#
# El token de enrolamiento se toma de STATUSSV_ENROLL_TOKEN o, si no está, se pide
# por la terminal sin mostrarlo. Nunca va como argumento: los argumentos de un
# proceso los puede leer cualquier usuario del sistema con ps.
#
# Correrlo de nuevo actualiza el binario y la unidad sin volver a enrolar. Para
# forzar un enrolamiento nuevo (con un token nuevo): --reenroll.
#
# El deploy (ADR-015) viene apagado. --enable-deploy lo prende en este servidor:
# escribe el drop-in 20-deploy.conf con los workdir de los proyectos declarados
# y revisa cada proyecto con deploy-check. Correr el script de nuevo, con el
# deploy prendido, actualiza los workdir (por ejemplo, después de declarar un
# proyecto nuevo). --disable-deploy lo apaga.
#
set -euo pipefail

readonly SERVICE="statussv-agent"
readonly SERVICE_USER="statussv"
readonly BINARY_DEST="/usr/local/bin/statussv-agent"
readonly UNIT_DEST="/etc/systemd/system/${SERVICE}.service"
readonly STATE_DIR="/var/lib/status-sv"
readonly STATE_FILE="${STATE_DIR}/agent.json"
readonly DROPIN_DIR="/etc/systemd/system/${SERVICE}.service.d"
readonly DROPIN_DOCKER="${DROPIN_DIR}/10-docker.conf"
readonly DROPIN_DEPLOY="${DROPIN_DIR}/20-deploy.conf"
# Las deploy keys, una por proyecto, y los host keys del servidor de git
# (ADR-015). Las pone una persona.
readonly KEYS_DIR="/etc/status-sv/keys"
# Los proyectos del servidor los declara una persona; el agent solo los lee
# (ADR-012).
readonly PROJECTS_DIR="/etc/status-sv/projects.d"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
readonly SCRIPT_DIR
readonly UNIT_SRC="${SCRIPT_DIR}/systemd/${SERVICE}.service"

error() { printf 'error: %s\n' "$*" >&2; exit 1; }
paso() { printf '\n==> %s\n' "$*"; }

uso() {
  cat >&2 <<'USO'
Uso: install-agent.sh [--reenroll] [--enable-deploy | --disable-deploy] RUTA_AL_BINARIO

Variables de entorno:
  STATUSSV_SERVER_URL     dirección del control plane (obligatoria al enrolar)
  STATUSSV_ENROLL_TOKEN   token de enrolamiento (si falta, se pide por la terminal)
USO
  exit 2
}

# --- Argumentos ---

REENROLL=false
# DEPLOY es enable, disable o keep (deja como está).
DEPLOY=keep
BINARY_SRC=""
for arg in "$@"; do
  case "${arg}" in
    --reenroll) REENROLL=true ;;
    --enable-deploy) DEPLOY=enable ;;
    --disable-deploy) DEPLOY=disable ;;
    -h|--help) uso ;;
    -*) error "opción desconocida: ${arg}" ;;
    *) BINARY_SRC="${arg}" ;;
  esac
done
[ -n "${BINARY_SRC}" ] || uso

# --- Comprobaciones previas: fallar antes de tocar nada ---

paso "comprobando el sistema"
[ "$(id -u)" -eq 0 ] || error "hay que correrlo como root"
[ "$(uname -s)" = "Linux" ] || error "el agent solo corre en Linux"
command -v systemctl >/dev/null || error "no se encontró systemd"
command -v runuser >/dev/null || error "no se encontró runuser (util-linux)"
[ -f "${BINARY_SRC}" ] || error "no existe el binario: ${BINARY_SRC}"
# Un binario copiado desde Windows llega sin el permiso de ejecución. Sin este
# chequeo, el error sería "el binario no corre en este servidor", que manda a
# buscar por el lado equivocado.
[ -x "${BINARY_SRC}" ] || error "al binario le falta el permiso de ejecución: chmod +x ${BINARY_SRC}"
[ -f "${UNIT_SRC}" ] || error "no se encontró la unidad de systemd en ${UNIT_SRC}"

# Que el binario corra de verdad en esta máquina, antes de instalarlo.
if ! VERSION="$("${BINARY_SRC}" version 2>&1)"; then
  error "el binario no corre en este servidor: ${VERSION}"
fi
echo "binario: ${VERSION}"

# --- Usuario del servicio ---

paso "usuario ${SERVICE_USER}"
if id "${SERVICE_USER}" >/dev/null 2>&1; then
  echo "ya existe"
else
  # De sistema, sin home y sin shell: no hay forma de iniciar sesión con él.
  useradd --system --no-create-home --home-dir /nonexistent \
    --shell /usr/sbin/nologin "${SERVICE_USER}"
  echo "creado"
fi

# --- Binario, unidad y directorio de estado ---

paso "instalando el binario y la unidad"
# El servicio corre con otro usuario que no puede modificar su propio binario.
install -o root -g root -m 0755 "${BINARY_SRC}" "${BINARY_DEST}"
install -o root -g root -m 0644 "${UNIT_SRC}" "${UNIT_DEST}"
install -d -o "${SERVICE_USER}" -g "${SERVICE_USER}" -m 0700 "${STATE_DIR}"
install -d -o root -g root -m 0755 "$(dirname "${PROJECTS_DIR}")"
# Si ya existe, "install -d" no toca lo que tiene adentro.
install -d -o root -g "${SERVICE_USER}" -m 0750 "${PROJECTS_DIR}"
echo "${BINARY_DEST}"
echo "${UNIT_DEST}"
echo "${PROJECTS_DIR}"

# --- Acceso a Docker ---

# Para ver los contenedores, el agent necesita el socket de Docker, y al socket
# se llega por el grupo docker. Va como drop-in y no en la unidad porque un
# servidor sin Docker no tiene ese grupo, y systemd no arranca un servicio cuyo
# grupo suplementario no existe. El grupo docker equivale a root: el porqué de
# aceptarlo está en docs/adr/ADR-011-acceso-a-docker-y-journald.md.
paso "acceso a Docker"
if getent group docker >/dev/null; then
  install -d -o root -g root -m 0755 "${DROPIN_DIR}"
  install -o root -g root -m 0644 /dev/null "${DROPIN_DOCKER}"
  cat > "${DROPIN_DOCKER}" <<'DROPIN'
# Lo escribe deploy/install-agent.sh; no editar a mano.
# El agent entra al grupo docker para leer /run/docker.sock (ADR-011).
[Service]
SupplementaryGroups=docker
DROPIN
  echo "el agent entra al grupo docker"
else
  # Si Docker se desinstaló, hay que sacar el drop-in o el servicio no arranca.
  rm -f "${DROPIN_DOCKER}"
  echo "este servidor no tiene el grupo docker: el agent no va a ver contenedores"
  echo "si instalás Docker después, corré este script de nuevo"
fi

# --- Deploy ---

# El deploy viene apagado (ADR-015): lo prende una persona acá, con
# --enable-deploy. El drop-in le da al agent lo justo para desplegar: el
# interruptor, escritura en el workdir de cada proyecto declarado (el guion
# hace que un workdir borrado no impida arrancar), un umask que deja legible
# lo que escribe git, y más memoria y tareas para docker compose y git, que
# corren como hijos del agent.
DEPLOY_ON=false
paso "deploy"
if [ "${DEPLOY}" = disable ]; then
  rm -f "${DROPIN_DEPLOY}"
  echo "apagado"
elif [ "${DEPLOY}" = enable ] || [ -f "${DROPIN_DEPLOY}" ]; then
  getent group docker >/dev/null || error "para desplegar hace falta Docker (no existe el grupo docker)"
  command -v git >/dev/null || error "para desplegar hace falta git"
  docker compose version >/dev/null 2>&1 || error "para desplegar hace falta el plugin de Docker Compose"

  install -d -o root -g "${SERVICE_USER}" -m 0750 "${KEYS_DIR}"
  # deploy-paths lee los proyectos declarados; lo corre root, que es quien
  # puede leer el directorio aunque falte el grupo.
  if ! PATHS="$(STATUSSV_PROJECTS_DIR="${PROJECTS_DIR}" "${BINARY_DEST}" deploy-paths)"; then
    error "no se pudieron leer los proyectos declarados en ${PROJECTS_DIR}"
  fi

  install -d -o root -g root -m 0755 "${DROPIN_DIR}"
  install -o root -g root -m 0644 /dev/null "${DROPIN_DEPLOY}"
  {
    echo "# Lo escribe deploy/install-agent.sh --enable-deploy; no editar a mano."
    echo "# El porqué de cada renglón está en docs/adr/ADR-015-motor-de-deploy-docker.md."
    echo "[Service]"
    echo "Environment=STATUSSV_DEPLOY_ENABLED=true"
    echo "UMask=0002"
    echo "MemoryMax=512M"
    echo "TasksMax=256"
    while read -r _ workdir; do
      [ -n "${workdir}" ] && echo "ReadWritePaths=-${workdir}"
    done <<<"${PATHS}"
  } >"${DROPIN_DEPLOY}"
  DEPLOY_ON=true
  echo "prendido; proyectos que se pueden desplegar:"
  if [ -z "${PATHS}" ]; then
    echo "  ninguno todavía (declaralos en ${PROJECTS_DIR} y corré este script de nuevo)"
  else
    sed 's/^/  /' <<<"${PATHS}"
  fi
else
  echo "apagado (para prenderlo: --enable-deploy)"
fi

systemctl daemon-reload

# --- Enrolamiento ---

if [ -f "${STATE_FILE}" ] && [ "${REENROLL}" = false ]; then
  paso "el agent ya está enrolado; se conserva la credencial"
else
  paso "enrolando el agent"
  [ -n "${STATUSSV_SERVER_URL:-}" ] || error "falta STATUSSV_SERVER_URL"

  if [ -z "${STATUSSV_ENROLL_TOKEN:-}" ]; then
    [ -t 0 ] || error "falta STATUSSV_ENROLL_TOKEN y no hay terminal para pedirlo"
    read -rsp "Token de enrolamiento: " STATUSSV_ENROLL_TOKEN
    echo
  fi
  [ -n "${STATUSSV_ENROLL_TOKEN}" ] || error "el token está vacío"

  # Si ya había un agent corriendo, se lo para para que no compita con el nuevo
  # enrolamiento.
  systemctl stop "${SERVICE}" 2>/dev/null || true

  # Las variables viajan por el entorno y no como argumentos. runuser las
  # conserva: solo reemplaza HOME, SHELL, USER y LOGNAME.
  export STATUSSV_SERVER_URL STATUSSV_ENROLL_TOKEN
  export STATUSSV_STATE_FILE="${STATE_FILE}"
  export STATUSSV_LOG_FORMAT=text

  ENROLL_ARGS=(enroll)
  [ "${REENROLL}" = true ] && ENROLL_ARGS+=(--force)

  if ! runuser -u "${SERVICE_USER}" -- "${BINARY_DEST}" "${ENROLL_ARGS[@]}"; then
    unset STATUSSV_ENROLL_TOKEN
    error "falló el enrolamiento; el token puede estar vencido o ya usado"
  fi
  unset STATUSSV_ENROLL_TOKEN
fi

# El archivo de estado tiene la credencial: dueño y permisos, verificados.
[ -f "${STATE_FILE}" ] || error "no quedó el archivo de estado en ${STATE_FILE}"
chown "${SERVICE_USER}:${SERVICE_USER}" "${STATE_FILE}"
chmod 0600 "${STATE_FILE}"

# --- Servicio ---

paso "arrancando el servicio"
systemctl enable "${SERVICE}" >/dev/null 2>&1
systemctl restart "${SERVICE}"

# Se le da un momento para que conecte y se mira que siga vivo.
sleep 5
if ! systemctl is-active --quiet "${SERVICE}"; then
  journalctl -u "${SERVICE}" -n 30 --no-pager >&2 || true
  error "el servicio no quedó corriendo"
fi

# Con el deploy prendido, cada proyecto se revisa como lo va a ver el agent:
# con su usuario. No frena la instalación: dice qué falta arreglar.
if [ "${DEPLOY_ON}" = true ] && [ -n "${PATHS}" ]; then
  while read -r proyecto _; do
    [ -n "${proyecto}" ] || continue
    paso "revisando el deploy de ${proyecto}"
    # Con el grupo docker, como lo tiene el servicio por su drop-in. El -g hace
    # falta: sin él, runuser usa el primer -G como grupo primario y statussv
    # pierde su propio grupo (y la lectura de projects.d).
    # STATUSSV_DEPLOY_ALLOW_FILE_REPOS pasa solo si viene del entorno: es para
    # el laboratorio, que usa un repo file://.
    runuser -u "${SERVICE_USER}" -g "${SERVICE_USER}" -G docker -- env STATUSSV_STATE_FILE="${STATE_FILE}" \
      STATUSSV_PROJECTS_DIR="${PROJECTS_DIR}" STATUSSV_DEPLOY_ENABLED=true \
      ${STATUSSV_DEPLOY_ALLOW_FILE_REPOS:+STATUSSV_DEPLOY_ALLOW_FILE_REPOS="${STATUSSV_DEPLOY_ALLOW_FILE_REPOS}"} \
      "${BINARY_DEST}" deploy-check "${proyecto}" \
      || echo "(el deploy de ${proyecto} no va a andar hasta arreglar lo de arriba; ver docs/runbook-despliegue.md)"
  done <<<"${PATHS}"
fi

paso "listo"
systemctl --no-pager --lines=0 status "${SERVICE}" | head -5
echo
echo "Registros en vivo:  journalctl -u ${SERVICE} -f"
