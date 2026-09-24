#!/bin/bash
# Laboratorio de la unidad del agent: corre la unidad ENDURECIDA DE VERDAD bajo
# systemd, cambiando solo qué ejecuta, y prueba lo que el Sprint 2 necesita.
# Se corre como root donde systemd sea PID 1 (WSL con systemd=true, o un
# contenedor privilegiado). Deja el sistema como estaba: ver limpieza.sh.
set -uo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
NAME=statussv-sonda
UNIT=/etc/systemd/system/${NAME}.service
DROPIN_DIR=/etc/systemd/system/${NAME}.service.d
MARCA=/tmp/.lab-creo-grupo-docker

echo "systemd: $(systemctl --version | head -1)"
echo "cgroup:  $(stat -fc %T /sys/fs/cgroup)"
echo

echo "== preparación =="
id statussv >/dev/null 2>&1 || useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin statussv
if getent group docker >/dev/null; then
  echo "el grupo docker ya existía"
else
  groupadd docker && touch "${MARCA}" && echo "grupo docker creado (se borra al limpiar)"
fi
for p in socat curl; do
  command -v "$p" >/dev/null || { echo "instalando ${p}"; DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$p" >/dev/null 2>&1; }
done
install -m 0755 "${DIR}/sonda.sh" /usr/local/bin/sonda.sh

# La unidad real, con dos cambios: qué ejecuta y que corre una sola vez.
sed -e 's|^ExecStart=.*|ExecStart=/usr/local/bin/sonda.sh|' \
    -e 's|^Type=simple|Type=oneshot|' \
    -e '/^Restart=/d' -e '/^RestartSec=/d' \
    "${DIR}/../systemd/statussv-agent.service" > "${UNIT}"

# El socket de Docker, simulado: root:docker, modo 660, responde 200.
rm -f /run/docker.sock
install -m 0755 "${DIR}/docker-falso.sh" /usr/local/bin/docker-falso.sh
setsid socat UNIX-LISTEN:/run/docker.sock,fork,mode=660,group=docker EXEC:/usr/local/bin/docker-falso.sh >/dev/null 2>&1 &
sleep 1
ls -l /run/docker.sock

corre() { # $1 = título
  systemctl reset-failed ${NAME} 2>/dev/null
  local desde; desde="$(date '+%Y-%m-%d %H:%M:%S')"
  systemctl start ${NAME} >/dev/null 2>&1
  local rc=$?
  sleep 1
  journalctl -u ${NAME} --since "${desde}" -o cat --no-pager 2>/dev/null
  return $rc
}

echo
echo "== caso A: sin drop-in (servidor sin Docker) =="
rm -rf "${DROPIN_DIR}"; systemctl daemon-reload
corre

echo
echo "== caso B: con drop-in y grupo docker existente =="
mkdir -p "${DROPIN_DIR}"
printf '[Service]\nSupplementaryGroups=docker\n' > "${DROPIN_DIR}/10-docker.conf"
systemctl daemon-reload
corre

echo
echo "== caso C: drop-in con un grupo inexistente (lo que evita el instalador) =="
printf '[Service]\nSupplementaryGroups=nohaydocker\n' > "${DROPIN_DIR}/10-docker.conf"
systemctl daemon-reload
if corre >/dev/null 2>&1; then
  echo "RESULTADO: arrancó igual; el drop-in condicional no haría falta"
else
  echo "RESULTADO: no arranca, como se esperaba"
  systemctl status ${NAME} --no-pager --lines=4 2>&1 | tail -4
fi
