#!/bin/bash
# Deja WSL como estaba antes del laboratorio.
set -uo pipefail
NAME=statussv-sonda
systemctl stop ${NAME} 2>/dev/null
systemctl reset-failed ${NAME} 2>/dev/null
rm -rf /etc/systemd/system/${NAME}.service /etc/systemd/system/${NAME}.service.d
systemctl daemon-reload
pkill -f 'UNIX-LISTEN:/run/docker.sock' 2>/dev/null; rm -f /usr/local/bin/docker-falso.sh
rm -f /run/docker.sock /usr/local/bin/sonda.sh /etc/sonda
rm -rf /var/lib/status-sv
id statussv >/dev/null 2>&1 && userdel statussv 2>/dev/null
[ -f /tmp/.lab-creo-grupo-docker ] && groupdel docker 2>/dev/null && rm -f /tmp/.lab-creo-grupo-docker
echo "limpio:"
echo "  unidad:  $(ls /etc/systemd/system/statussv-sonda* 2>/dev/null || echo 'no queda')"
echo "  usuario: $(id statussv 2>&1 | head -1)"
echo "  grupo:   $(getent group docker || echo 'no queda')"
echo "  socket:  $(ls /run/docker.sock 2>/dev/null || echo 'no queda')"
