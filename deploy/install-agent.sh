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
set -euo pipefail

readonly SERVICE="statussv-agent"
readonly SERVICE_USER="statussv"
readonly BINARY_DEST="/usr/local/bin/statussv-agent"
readonly UNIT_DEST="/etc/systemd/system/${SERVICE}.service"
readonly STATE_DIR="/var/lib/status-sv"
readonly STATE_FILE="${STATE_DIR}/agent.json"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
readonly SCRIPT_DIR
readonly UNIT_SRC="${SCRIPT_DIR}/systemd/${SERVICE}.service"

error() { printf 'error: %s\n' "$*" >&2; exit 1; }
paso() { printf '\n==> %s\n' "$*"; }

uso() {
  cat >&2 <<'USO'
Uso: install-agent.sh [--reenroll] RUTA_AL_BINARIO

Variables de entorno:
  STATUSSV_SERVER_URL     dirección del control plane (obligatoria al enrolar)
  STATUSSV_ENROLL_TOKEN   token de enrolamiento (si falta, se pide por la terminal)
USO
  exit 2
}

# --- Argumentos ---

REENROLL=false
BINARY_SRC=""
for arg in "$@"; do
  case "${arg}" in
    --reenroll) REENROLL=true ;;
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
echo "${BINARY_DEST}"
echo "${UNIT_DEST}"
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

paso "listo"
systemctl --no-pager --lines=0 status "${SERVICE}" | head -5
echo
echo "Registros en vivo:  journalctl -u ${SERVICE} -f"
