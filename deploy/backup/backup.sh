#!/bin/sh
# Backup del control plane: la base, la clave TOTP y el .env, cifrados con
# restic en un almacenamiento fuera del servidor (Cloudflare R2 en producción).
#
# Corre adentro de la imagen status-sv-backup (deploy/Dockerfile, target
# backup), como el servicio "backup" de deploy/docker-compose.prod.yml:
#
#   backup.sh servicio        un backup al arrancar y después uno por día, a
#                             la hora BACKUP_HORA_UTC; no termina
#   backup.sh una-vez         un backup, la retención y el chequeo, y termina
#   backup.sh init            crea el repositorio (una sola vez por destino)
#   backup.sh snapshots       lista las copias
#   backup.sh restaurar [ID] DESTINO
#                             baja una copia (latest si falta el ID) a DESTINO
#
# Lo que entra en cada copia, con estas rutas adentro del snapshot:
#
#   /work/statussv.dump       pg_dump en formato custom, sin comprimir: restic
#                             comprime y deduplica, y un dump ya comprimido
#                             cambiaría entero de un día al otro
#   /run/secrets/totp_key     sin ella, los segundos factores de la base no se
#                             pueden descifrar
#   /backup/env/.env          la configuración del servidor
#
# Variables:
#   RESTIC_REPOSITORY, RESTIC_PASSWORD_FILE    el repositorio y su contraseña
#   AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY   las credenciales del bucket
#   PGHOST, PGUSER, PGDATABASE, PGPASSWORD     la base
#   BACKUP_HORA_UTC (06)        la hora del backup diario
#   BACKUP_PING_URL             opcional: se le avisa cada backup bueno, y a
#                               su /fail cada uno que falla (healthchecks.io)
#   BACKUP_ENV_FILE (/backup/env/.env), TOTP_KEY_FILE (/run/secrets/totp_key)
#   WORK_DIR (/work)
set -eu

HORA_UTC="${BACKUP_HORA_UTC:-06}"
ENV_FILE="${BACKUP_ENV_FILE:-/backup/env/.env}"
TOTP_KEY_FILE="${TOTP_KEY_FILE:-/run/secrets/totp_key}"
WORK_DIR="${WORK_DIR:-/work}"
DUMP="${WORK_DIR}/statussv.dump"
# Lo mira el healthcheck del contenedor: si la última copia buena es de hace
# más de un día y pico, el servicio queda unhealthy.
ULTIMO_OK="${WORK_DIR}/ultimo-ok"

# Retención: una por día durante una semana, una por semana durante un mes y
# una por mes durante medio año.
RETENCION="--keep-daily 7 --keep-weekly 4 --keep-monthly 6"

log() {
  echo "[backup] $(date -u +%Y-%m-%dT%H:%M:%SZ) $*"
}

falta() {
  log "ERROR: $*"
  exit 2
}

revisar_config() {
  [ -n "${RESTIC_REPOSITORY:-}" ] || falta "falta RESTIC_REPOSITORY (STATUSSV_BACKUP_REPOSITORY en deploy/.env)"
  [ -s "${RESTIC_PASSWORD_FILE:-/nada}" ] || falta "falta la contraseña del repositorio (deploy/secrets/restic.password)"
}

avisar() {
  # El aviso es opcional y nunca puede romper el backup.
  [ -n "${BACKUP_PING_URL:-}" ] || return 0
  wget -q -T 10 -O /dev/null "${BACKUP_PING_URL}$1" 2>/dev/null || log "no se pudo avisar a BACKUP_PING_URL"
}

volcar_base() {
  mkdir -p "${WORK_DIR}"
  tmp="${DUMP}.tmp"
  rm -f "${tmp}"
  log "volcando la base ${PGDATABASE:-?} de ${PGHOST:-?}"
  if ! pg_dump --format=custom --compress=0 --file="${tmp}"; then
    rm -f "${tmp}"
    log "ERROR: falló pg_dump"
    return 1
  fi
  # Un volcado truncado no lo lee pg_restore. Mejor enterarse hoy que el día
  # que haga falta.
  if ! pg_restore --list "${tmp}" >/dev/null; then
    rm -f "${tmp}"
    log "ERROR: pg_restore no puede leer el volcado"
    return 1
  fi
  mv "${tmp}" "${DUMP}"
}

hacer_backup() {
  revisar_config
  [ -r "${TOTP_KEY_FILE}" ] || { log "ERROR: no se puede leer ${TOTP_KEY_FILE}"; return 1; }
  [ -r "${ENV_FILE}" ] || { log "ERROR: no se puede leer ${ENV_FILE}"; return 1; }

  volcar_base || return 1

  log "subiendo la copia a ${RESTIC_REPOSITORY%%\?*}"
  restic backup --quiet --host status-sv --tag diario \
    "${DUMP}" "${TOTP_KEY_FILE}" "${ENV_FILE}" || { log "ERROR: falló restic backup"; return 1; }

  log "aplicando la retención"
  # shellcheck disable=SC2086 # RETENCION son varias opciones a propósito.
  restic forget --quiet --host status-sv --prune ${RETENCION} || { log "ERROR: falló restic forget"; return 1; }

  # Lee de verdad una parte de los datos, no solo los índices: así un archivo
  # dañado en el bucket aparece antes de necesitarlo.
  log "revisando el repositorio"
  restic check --quiet --read-data-subset=10% || { log "ERROR: falló restic check"; return 1; }

  rm -f "${DUMP}"
  touch "${ULTIMO_OK}"
  log "OK: $(restic snapshots --host status-sv --compact --latest 1 | sed -n '3p')"
}

una_vez() {
  if hacer_backup; then
    avisar ""
  else
    avisar "/fail"
    return 1
  fi
}

servicio() {
  revisar_config
  log "servicio: una copia al arrancar y otra cada día a las ${HORA_UTC}:00 UTC"
  una_vez || log "la copia falló; se reintenta en la próxima vuelta"
  hecho="$(date -u +%F)"
  while true; do
    sleep 60
    hoy="$(date -u +%F)"
    if [ "$(date -u +%H)" = "${HORA_UTC}" ] && [ "${hoy}" != "${hecho}" ]; then
      hecho="${hoy}"
      una_vez || log "la copia falló; se reintenta mañana"
    fi
  done
}

case "${1:-servicio}" in
  servicio) servicio ;;
  una-vez) una_vez ;;
  init)
    revisar_config
    restic init
    ;;
  snapshots)
    revisar_config
    restic snapshots
    ;;
  restaurar)
    revisar_config
    if [ $# -eq 3 ]; then
      id="$2"
      destino="$3"
    elif [ $# -eq 2 ]; then
      id=latest
      destino="$2"
    else
      falta "uso: backup.sh restaurar [ID] DESTINO"
    fi
    restic restore "${id}" --host status-sv --target "${destino}"
    log "restaurado ${id} en ${destino}"
    ;;
  *)
    falta "subcomando desconocido: $1 (servicio, una-vez, init, snapshots o restaurar)"
    ;;
esac
