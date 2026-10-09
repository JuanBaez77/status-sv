#!/bin/bash
# Laboratorio del deploy (ADR-015): corre la unidad ENDURECIDA DE VERDAD del
# agent bajo systemd, con sonda-deploy.sh en lugar del agent, y prueba lo que el
# motor de deploy va a hacer: git con la deploy key, docker compose y la API de
# Docker, como statussv y con un Docker de verdad.
#
# Necesita systemd como PID 1, Docker con el plugin de Compose, git y ssh. Se
# corre como root en una máquina descartable: el runner del CI (job
# "laboratorio") o WSL con Docker instalado adentro. Crea y borra lo suyo: el
# usuario statussv, /srv/lab-*, /etc/status-sv y la unidad statussv-sonda.
#
#   sudo ./deploy/laboratorio/deploy.sh
#
# Casos:
#   A  sin 20-deploy.conf: el workdir es de solo lectura y el fetch falla.
#   C  con 20-deploy.conf pero los techos de la unidad base (64M, 64 tareas):
#      solo se mide, para saber si hacía falta subirlos.
#   B  con 20-deploy.conf completo: todo lo que el motor necesita tiene que andar.
set -uo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
NAME=statussv-sonda
UNIT=/etc/systemd/system/${NAME}.service
DROPIN_DIR=/etc/systemd/system/${NAME}.service.d
ORIGEN=/srv/lab-origen.git
WD=/srv/lab-proyecto
MARCA_USUARIO=/tmp/.lab-deploy-creo-statussv

rojo()  { printf '\033[31m%s\033[0m\n' "$*" >&2; }
verde() { printf '\033[32m%s\033[0m\n' "$*"; }
paso()  { printf '\n== %s ==\n' "$*"; }

[ "$(id -u)" -eq 0 ] || { rojo "hay que correrlo como root"; exit 2; }
for p in systemctl docker git ssh ssh-keygen curl; do
  command -v "$p" >/dev/null || { rojo "falta ${p}"; exit 2; }
done
docker compose version >/dev/null 2>&1 || { rojo "falta el plugin de Docker Compose"; exit 2; }
getent group docker >/dev/null || { rojo "no existe el grupo docker"; exit 2; }

limpiar() {
  paso "limpieza"
  systemctl stop ${NAME} 2>/dev/null
  systemctl reset-failed ${NAME} 2>/dev/null
  rm -rf "${UNIT}" "${DROPIN_DIR}"
  # Lo que deja el instalador en el caso D.
  systemctl disable --now statussv-agent >/dev/null 2>&1
  rm -rf /etc/systemd/system/statussv-agent.service /etc/systemd/system/statussv-agent.service.d /usr/local/bin/statussv-agent
  systemctl daemon-reload
  [ -d "${WD}" ] && docker compose --project-directory "${WD}" -f "${WD}/compose.yml" -p lab down --rmi local >/dev/null 2>&1
  docker image rm statussv-rollback/lab-web:sonda >/dev/null 2>&1
  rm -rf "${ORIGEN}" "${WD}" /srv/otro-proyecto /etc/status-sv /var/lib/status-sv /usr/local/bin/sonda-deploy.sh
  if [ -f "${MARCA_USUARIO}" ]; then
    userdel statussv 2>/dev/null
    rm -f "${MARCA_USUARIO}"
  fi
  echo "listo"
}
trap limpiar EXIT

echo "systemd: $(systemctl --version | head -1)"
echo "docker:  $(docker version --format '{{.Server.Version}}')"
echo "compose: $(docker compose version --short)"
echo "git:     $(git --version)"

# --- Preparación ---

paso "preparación"
if ! id statussv >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin statussv
  touch "${MARCA_USUARIO}"
fi

# Un proyecto mínimo con dos commits: v1 es lo que corre, v2 suma un archivo.
TMP="$(mktemp -d)"
G=(git -c user.name=laboratorio -c user.email=lab@status-sv.invalid -c init.defaultBranch=main)
"${G[@]}" init -q "${TMP}"
printf 'FROM %s/busybox:1.37\n' "${IMAGENES:-docker.io/library}" >"${TMP}/Dockerfile"
cat >>"${TMP}/Dockerfile" <<'EOF'
COPY *.html /www/
CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]
EOF
cat >"${TMP}/compose.yml" <<'EOF'
services:
  web:
    build: .
    ports:
      - "127.0.0.1:18080:8080"
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/"]
      interval: 1s
      timeout: 2s
      retries: 20
EOF
echo "v1" >"${TMP}/index.html"
"${G[@]}" -C "${TMP}" add -A && "${G[@]}" -C "${TMP}" commit -qm v1
V1="$(git -C "${TMP}" rev-parse HEAD)"
echo "v2" >"${TMP}/nuevo.html"
"${G[@]}" -C "${TMP}" add -A && "${G[@]}" -C "${TMP}" commit -qm v2
V2="$(git -C "${TMP}" rev-parse HEAD)"
rm -rf "${ORIGEN}"
git clone -q --bare "${TMP}" "${ORIGEN}"
rm -rf "${TMP}"

# El workdir lo clonó root, como en los servidores de hoy, y está en v1.
rm -rf "${WD}"
git clone -q "file://${ORIGEN}" "${WD}"
git -C "${WD}" checkout -q -B main "${V1}"

# Permisos del runbook: el dueño sigue siendo root; statussv escribe por grupo
# en .git y en los directorios con archivos versionados, con setgid para que lo
# nuevo herede el grupo.
chgrp -R statussv "${WD}/.git"
chmod -R g+rwX "${WD}/.git"
find "${WD}/.git" -type d -exec chmod g+s {} +
{ echo .; git -C "${WD}" ls-tree -r -d --name-only HEAD; } | while read -r d; do
  chgrp statussv "${WD}/${d}" && chmod g+rwxs "${WD}/${d}"
done

# Un hook plantado en el workdir: el agent no lo tiene que ejecutar.
printf '#!/bin/sh\ntouch /tmp/hook-corrio\n' >"${WD}/.git/hooks/post-checkout"
chmod 755 "${WD}/.git/hooks/post-checkout"

# Otro proyecto al lado, que el agent no tiene que poder tocar.
mkdir -p /srv/otro-proyecto

# La deploy key y los host keys, como los deja una persona.
install -d -m 0755 /etc/status-sv /etc/status-sv/keys
ssh-keygen -q -t ed25519 -N '' -C 'status-sv laboratorio' -f /etc/status-sv/keys/lab
rm -f /etc/status-sv/keys/lab.pub
chown statussv:statussv /etc/status-sv/keys/lab
chmod 0400 /etc/status-sv/keys/lab
if ssh-keyscan -T 10 -t ed25519 github.com >/etc/status-sv/known_hosts 2>/dev/null && [ -s /etc/status-sv/known_hosts ]; then
  echo "known_hosts con github.com"
else
  : >/etc/status-sv/known_hosts
  echo "sin red hacia github.com: la prueba de ssh se salta"
fi
chmod 0644 /etc/status-sv/known_hosts

install -m 0755 "${DIR}/sonda-deploy.sh" /usr/local/bin/sonda-deploy.sh

# La unidad real, con dos cambios: qué ejecuta y que corre una sola vez.
sed -e 's|^ExecStart=.*|ExecStart=/usr/local/bin/sonda-deploy.sh|' \
    -e 's|^Type=simple|Type=oneshot|' \
    -e '/^Restart=/d' -e '/^RestartSec=/d' \
    "${DIR}/../systemd/statussv-agent.service" >"${UNIT}"

mkdir -p "${DROPIN_DIR}"
cat >"${DROPIN_DIR}/00-laboratorio.conf" <<EOF
[Service]
TimeoutStartSec=900
Environment=SONDA_WORKDIR=${WD}
Environment=SONDA_ORIGEN=file://${ORIGEN}
Environment=SONDA_V2=${V2}
EOF
# Lo que deja el instalador en un servidor con Docker (ADR-011).
printf '[Service]\nSupplementaryGroups=docker\n' >"${DROPIN_DIR}/10-docker.conf"

# corre arranca la unidad y muestra lo que dijo la sonda. Devuelve cómo salió.
corre() {
  systemctl daemon-reload
  systemctl reset-failed ${NAME} 2>/dev/null
  local desde rc
  desde="$(date '+%Y-%m-%d %H:%M:%S')"
  systemctl start ${NAME} >/dev/null 2>&1
  rc=$?
  sleep 1
  journalctl -u ${NAME} --since "${desde}" -o cat --no-pager 2>/dev/null | grep -Ev '^(Starting|Started|Finished|Stopped|Deactivated|Consumed|statussv-sonda)'
  return $rc
}

# vuelve_a_v1 deja el proyecto como antes de desplegar.
vuelve_a_v1() {
  docker compose --project-directory "${WD}" -f "${WD}/compose.yml" -p lab down >/dev/null 2>&1
  git -c "safe.directory=${WD}" -C "${WD}" checkout -q -B main "${V1}"
}

drop_in_deploy() { # $1 = con techos (si) o sin techos (no)
  {
    echo '[Service]'
    echo 'Environment=STATUSSV_DEPLOY_ENABLED=true'
    # El guion: si el workdir desaparece, la unidad igual arranca.
    echo "ReadWritePaths=-${WD}"
    echo 'UMask=0002'
    if [ "$1" = si ]; then
      echo 'MemoryMax=512M'
      echo 'TasksMax=256'
    fi
  } >"${DROPIN_DIR}/20-deploy.conf"
}

# caso_instalador es el caso D: deploy/install-agent.sh --enable-deploy con el
# agent de verdad sobre el mismo proyecto. Tiene que escribir 20-deploy.conf con
# el workdir declarado, dejar el servicio andando con el deploy prendido y que
# deploy-check, corrido como statussv, diga que se puede desplegar.
caso_instalador() {
  paso "caso D: el instalador con --enable-deploy"
  vuelve_a_v1
  install -d -m 0755 /etc/status-sv/projects.d
  cat >/etc/status-sv/projects.d/lab.yaml <<EOF
name: lab
environment: staging
repo: file://${ORIGEN}
branch: main
runtime: docker
compose_file: compose.yml
compose_project: lab
workdir: ${WD}
EOF
  # Un agent ya enrolado contra un control plane que no existe: el instalador
  # no enrola, y el servicio reintenta conectar sin morirse.
  install -d -o statussv -g statussv -m 0700 /var/lib/status-sv
  cat >/var/lib/status-sv/agent.json <<EOF
{"server_url":"http://127.0.0.1:9","server_id":"01924f9c-0000-7000-8000-0000000000aa","server_name":"lab","credential":"svagt_credencial-del-laboratorio","enrolled_at":"2026-09-30T12:00:00Z"}
EOF
  chown statussv:statussv /var/lib/status-sv/agent.json
  chmod 0600 /var/lib/status-sv/agent.json
  install -m 0755 "${AGENT_BIN}" /tmp/statussv-agent-lab

  local salida
  if ! salida="$(STATUSSV_DEPLOY_ALLOW_FILE_REPOS=true bash "${DIR}/../install-agent.sh" --enable-deploy /tmp/statussv-agent-lab 2>&1)"; then
    printf '%s\n' "${salida}"
    rojo "caso D: el instalador falló"
    return 1
  fi
  printf '%s\n' "${salida}" | sed -n '/==> deploy/,$p'

  local dropin=/etc/systemd/system/statussv-agent.service.d/20-deploy.conf fallas=0
  grep -qx "ReadWritePaths=-${WD}" "${dropin}" || { rojo "el drop-in no tiene el workdir"; fallas=1; }
  grep -qx 'Environment=STATUSSV_DEPLOY_ENABLED=true' "${dropin}" || { rojo "el drop-in no prende el deploy"; fallas=1; }
  grep -q 'lab se puede desplegar' <<<"${salida}" || { rojo "deploy-check no aprobó el proyecto"; fallas=1; }
  systemctl is-active --quiet statussv-agent || { rojo "el servicio no quedó andando"; fallas=1; }
  journalctl -u statussv-agent --no-pager -o cat | grep -q 'el deploy está prendido' || { rojo "el agent no dice que el deploy está prendido"; fallas=1; }

  # Apagarlo saca el drop-in.
  bash "${DIR}/../install-agent.sh" --disable-deploy /tmp/statussv-agent-lab >/dev/null 2>&1
  [ ! -e "${dropin}" ] || { rojo "--disable-deploy no sacó el drop-in"; fallas=1; }
  rm -f /tmp/statussv-agent-lab

  if [ "${fallas}" -eq 0 ]; then verde "caso D: el instalador prende y apaga el deploy"; else rojo "caso D: algo no dio lo esperado"; fi
  return "${fallas}"
}

resultado=0

paso "caso A: sin el drop-in de deploy"
rm -f "${DROPIN_DIR}/20-deploy.conf" "${DROPIN_DIR}/30-espera.conf"
printf '[Service]\nEnvironment=SONDA_ESPERA=sin-deploy\n' >"${DROPIN_DIR}/30-espera.conf"
if corre; then verde "caso A: como se esperaba"; else rojo "caso A: algo no dio lo esperado"; resultado=1; fi

paso "caso C: con el drop-in, pero con los techos de la unidad base (solo se mide)"
drop_in_deploy no
printf '[Service]\nEnvironment=SONDA_ESPERA=deploy\n' >"${DROPIN_DIR}/30-espera.conf"
if corre; then
  echo "caso C: anduvo con 64M y 64 tareas"
else
  echo "caso C: no alcanza con los techos de la unidad base"
  systemctl status ${NAME} --no-pager --lines=0 2>&1 | grep -E 'Active|Memory|Tasks' || true
fi
vuelve_a_v1

paso "caso B: con el drop-in de deploy completo"
drop_in_deploy si
if corre; then verde "caso B: todo lo que el motor necesita anda"; else rojo "caso B: algo no dio lo esperado"; resultado=1; fi

if [ -n "${AGENT_BIN:-}" ]; then
  caso_instalador || resultado=1
else
  echo
  echo "caso D: se salta (AGENT_BIN no apunta a un binario del agent)"
fi

exit "${resultado}"
