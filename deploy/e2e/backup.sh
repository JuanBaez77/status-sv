#!/usr/bin/env bash
#
# Backup y restore de punta a punta: que la copia diaria del control plane
# (deploy/backup/backup.sh) se pueda usar el día que haga falta.
#
#   1. Un control plane de origen, con las migraciones y un usuario marcador.
#   2. Dos backups a un repositorio de restic local, con las mismas
#      restricciones que en producción: la clave TOTP de otro usuario (0400),
#      el .env de root (0600), raíz de solo lectura y ninguna capacidad salvo
#      DAC_READ_SEARCH.
#   3. Con una contraseña equivocada, la copia no se abre.
#   4. Se baja la última copia y se corre el ensayo de restore
#      (deploy/backup/ensayo-restore.sh): base nueva, migraciones, /readyz.
#   5. Lo restaurado trae el usuario marcador y la misma clave TOTP.
#
# Se corre igual en local y en CI:
#
#   ./deploy/e2e/backup.sh
#
set -euo pipefail

cd "$(dirname "$0")/../.."

IMAGENES="${IMAGENES:-docker.io/library}"
export IMAGENES
SERVER_IMG=status-sv-server:backup-e2e
BACKUP_IMG=status-sv-backup:backup-e2e
ORIGEN=status-sv-backup-origen
ENSAYO=status-sv-backup-ensayo
VOL_REPO=status-sv-backup-e2e-repo
VOL_WORK=status-sv-backup-e2e-work
VOL_CACHE=status-sv-backup-e2e-cache
MARCADOR="marcador-$(date +%s)"

rojo() { printf '\033[31m%s\033[0m\n' "$*" >&2; }
verde() { printf '\033[32m%s\033[0m\n' "$*"; }
paso() { printf '\n=== %s ===\n' "$*"; }

TRABAJO="$(mktemp -d)"
export STATUSSV_ENSAYO_IMAGEN="${SERVER_IMG}"
export STATUSSV_ENSAYO_TOTP_KEY="${TRABAJO}/totp.key"
ORIGEN_COMPOSE=(docker compose -p "${ORIGEN}" -f deploy/backup/docker-compose.ensayo.yml)
ENSAYO_COMPOSE=(docker compose -p "${ENSAYO}" -f deploy/backup/docker-compose.ensayo.yml)

# Los archivos de la prueba son de root o del usuario del control plane, como en
# el servidor: se tocan con un contenedor, así no hace falta sudo.
como_root() {
  docker run --rm -v "${TRABAJO}:/t" "${IMAGENES}/alpine:3.22" sh -c "$1"
}

limpiar() {
  local salida=$?
  if [ $salida -ne 0 ]; then
    rojo "la prueba falló; últimos registros:"
    "${ORIGEN_COMPOSE[@]}" logs --tail 20 2>&1 || true
  fi
  paso "limpiando"
  "${ORIGEN_COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  STATUSSV_ENSAYO_TOTP_KEY="${TRABAJO}/restaurado/run/secrets/totp_key" \
    "${ENSAYO_COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  docker volume rm -f "${VOL_REPO}" "${VOL_WORK}" "${VOL_CACHE}" >/dev/null 2>&1 || true
  como_root 'rm -rf /t/* /t/.[!.]*' >/dev/null 2>&1 || true
  rmdir "${TRABAJO}" 2>/dev/null || true
  exit $salida
}
trap limpiar EXIT

# backup corre la imagen de backup con las restricciones de producción, en la
# red del control plane de origen.
backup() {
  local password="$1"
  shift
  docker run --rm \
    --network "${ORIGEN}_default" \
    --read-only --tmpfs /tmp \
    --cap-drop ALL --cap-add DAC_READ_SEARCH \
    --security-opt no-new-privileges:true \
    -e PGHOST=postgres -e PGUSER=statussv -e PGDATABASE=statussv -e PGPASSWORD=ensayo \
    -e RESTIC_REPOSITORY=/repo \
    -e RESTIC_PASSWORD_FILE=/run/secrets/restic_password \
    -v "${TRABAJO}/totp.key:/run/secrets/totp_key:ro" \
    -v "${TRABAJO}/${password}:/run/secrets/restic_password:ro" \
    -v "${TRABAJO}/env:/backup/env/.env:ro" \
    -v "${VOL_REPO}:/repo" -v "${VOL_WORK}:/work" -v "${VOL_CACHE}:/cache" \
    "$@"
}

paso "Imágenes del control plane y del backup"
docker build -q -f deploy/Dockerfile --target server --build-arg IMAGENES -t "${SERVER_IMG}" . >/dev/null
docker build -q -f deploy/Dockerfile --target backup --build-arg IMAGENES -t "${BACKUP_IMG}" . >/dev/null
verde "ok: imágenes listas"

paso "Secretos con los dueños y permisos de producción"
openssl rand -base64 32 >"${TRABAJO}/totp.key"
openssl rand -base64 32 >"${TRABAJO}/restic.password"
openssl rand -base64 32 >"${TRABAJO}/restic.equivocada"
printf 'POSTGRES_PASSWORD=ensayo\nSTATUSSV_DOMAIN=backup-e2e.invalid\n' >"${TRABAJO}/env"
como_root 'chown 10001:10001 /t/totp.key && chmod 0400 /t/totp.key &&
  chown 0:0 /t/env /t/restic.password /t/restic.equivocada &&
  chmod 0600 /t/env /t/restic.password /t/restic.equivocada'
verde "ok: totp.key de 10001 (0400), .env y contraseñas de root (0600)"

paso "Control plane de origen con un usuario marcador"
"${ORIGEN_COMPOSE[@]}" up -d --wait postgres
"${ORIGEN_COMPOSE[@]}" run --rm --no-deps server migrate up
"${ORIGEN_COMPOSE[@]}" up -d --wait server
printf '%s\n' "$(openssl rand -hex 16)" |
  "${ORIGEN_COMPOSE[@]}" exec -T server /usr/local/bin/server user-add --username "${MARCADOR}" >/dev/null
verde "ok: usuario ${MARCADOR} creado"

paso "Dos backups"
backup restic.password "${BACKUP_IMG}" init
backup restic.password "${BACKUP_IMG}" una-vez
backup restic.password "${BACKUP_IMG}" una-vez
copias="$(backup restic.password --entrypoint restic "${BACKUP_IMG}" snapshots --json --host status-sv |
  grep -o '"short_id"' | wc -l)"
[ "${copias}" -eq 2 ] || { rojo "se esperaban 2 copias y hay ${copias}"; exit 1; }
docker run --rm -v "${VOL_WORK}:/work" "${IMAGENES}/alpine:3.22" test -f /work/ultimo-ok
verde "ok: 2 copias en el repositorio, y la marca de la última buena"

paso "Con la contraseña equivocada no se abre"
if backup restic.equivocada "${BACKUP_IMG}" snapshots >/dev/null 2>&1; then
  rojo "el repositorio se abrió con otra contraseña"
  exit 1
fi
verde "ok: rechazada"

paso "Bajar la última copia"
mkdir "${TRABAJO}/restaurado"
# Restaurar es a mano y necesita más que la copia diaria: escribir donde le
# digan y devolverle a cada archivo su dueño (la clave TOTP es de 10001).
backup restic.password --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
  -v "${TRABAJO}/restaurado:/restore" "${BACKUP_IMG}" restaurar /restore
como_root 'test -s /t/restaurado/work/statussv.dump && test -s /t/restaurado/backup/env/.env &&
  cmp /t/totp.key /t/restaurado/run/secrets/totp_key'
verde "ok: volcado, .env y la misma clave TOTP"

paso "Ensayo de restore"
ENSAYO_PROYECTO="${ENSAYO}" ENSAYO_DEJAR=1 ./deploy/backup/ensayo-restore.sh "${TRABAJO}/restaurado"

paso "Lo restaurado es lo de origen"
export STATUSSV_ENSAYO_TOTP_KEY="${TRABAJO}/restaurado/run/secrets/totp_key"
hay="$("${ENSAYO_COMPOSE[@]}" exec -T postgres psql -U statussv -d statussv -tAc \
  "SELECT count(*) FROM users WHERE username = '${MARCADOR}'")"
[ "${hay}" = 1 ] || { rojo "el usuario ${MARCADOR} no está en lo restaurado"; exit 1; }
verde "ok: el usuario ${MARCADOR} volvió con la copia"

verde "backup y restore de punta a punta: OK"
