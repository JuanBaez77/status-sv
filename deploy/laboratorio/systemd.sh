#!/bin/bash
# Laboratorio del deploy de systemd (ADR-019): corre la unidad ENDURECIDA DE
# VERDAD del agent bajo systemd, con sonda-systemd.sh en lugar del agent, y
# prueba el reparto que va a usar el motor para un proyecto systemd: el agent
# no reinicia ni ejecuta nada del proyecto; se lo pide a un ayudante root, por
# un socket unix, con operaciones fijas.
#
# El ayudante de verdad (statussv-agent unit-helper) todavía no existe: lo
# reemplaza ayudante-falso.sh, con la misma unidad, el mismo socket y el mismo
# endurecimiento. Lo que se mide es el mecanismo, no ese script.
#
# Necesita systemd como PID 1 y python3. No necesita Docker. Se corre como root
# en una máquina descartable: el runner del CI (job "laboratorio") o WSL. Crea y
# borra lo suyo.
#
#   sudo ./deploy/laboratorio/systemd.sh
set -uo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
NAME=statussv-sonda
UNIT=/etc/systemd/system/${NAME}.service
DROPIN_DIR=/etc/systemd/system/${NAME}.service.d
WD=/srv/lab-systemd
SOCKET=/run/status-sv/unit-helper.sock
REGLA=/etc/polkit-1/rules.d/49-statussv-laboratorio.rules
MARCA_USUARIO=/tmp/.lab-systemd-creo-statussv

rojo()  { printf '\033[31m%s\033[0m\n' "$*" >&2; }
verde() { printf '\033[32m%s\033[0m\n' "$*"; }
paso()  { printf '\n== %s ==\n' "$*"; }

[ "$(id -u)" -eq 0 ] || { rojo "hay que correrlo como root"; exit 2; }
for p in systemctl systemd-run journalctl python3 runuser; do
  command -v "$p" >/dev/null || { rojo "falta ${p}"; exit 2; }
done

limpiar() {
  paso "limpieza"
  systemctl stop ${NAME} lab-gunicorn lab-otra statussv-lab-ayudante.socket 2>/dev/null
  systemctl stop 'statussv-lab-ayudante@*.service' 'statussv-step-*.service' 2>/dev/null
  systemctl reset-failed ${NAME} 'statussv-lab-ayudante@*.service' 'statussv-step-*.service' 2>/dev/null
  rm -rf "${UNIT}" "${DROPIN_DIR}" "${REGLA}" \
    /etc/systemd/system/lab-gunicorn.service /etc/systemd/system/lab-otra.service \
    /etc/systemd/system/statussv-lab-ayudante.socket /etc/systemd/system/statussv-lab-ayudante@.service
  systemctl daemon-reload
  rm -rf "${WD}" /etc/lab-systemd /etc/status-sv /var/lib/status-sv /run/status-sv /etc/paso-lab \
    /usr/local/bin/sonda-systemd.sh /usr/local/bin/ayudante-falso.sh /usr/local/bin/cliente-ayudante.py
  userdel labapp 2>/dev/null
  if [ -f "${MARCA_USUARIO}" ]; then
    userdel statussv 2>/dev/null
    rm -f "${MARCA_USUARIO}"
  fi
  echo "listo"
}
trap limpiar EXIT

echo "systemd: $(systemctl --version | head -1)"
echo "polkit:  $(pkaction --version 2>/dev/null || echo 'no instalado')"
echo "python:  $(python3 --version 2>&1)"

# --- Preparación ---

paso "preparación"
if ! id statussv >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin statussv
  touch "${MARCA_USUARIO}"
fi
# El usuario del proyecto: con el que corre el servicio y sus pasos (run_as).
useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin labapp

# El workdir, con los permisos del runbook: de root, con escritura del grupo
# statussv para git. El usuario del proyecto escribe solo en lo suyo.
install -d -o root -g statussv -m 2775 "${WD}"
install -d -o labapp -g labapp -m 0755 "${WD}/salida"
cat >"${WD}/paso.sh" <<'EOF'
#!/bin/sh
# Lo que haría un `pip install`: código del repositorio corriendo en el servidor.
echo "usuario=$(id -un)"
echo "directorio=$(pwd)"
if [ -n "${LAB_SECRETO:-}" ]; then echo "entorno=presente"; else echo "entorno=ausente"; fi
echo "cgroup=$(cut -d: -f3 /proc/self/cgroup)"
if cat /var/lib/status-sv/agent.json >/dev/null 2>&1; then echo "credencial=legible"; else echo "credencial=no"; fi
if cat /etc/status-sv/keys/lab >/dev/null 2>&1; then echo "llave=legible"; else echo "llave=no"; fi
if touch salida/construido.txt 2>/dev/null; then echo "escritura=ok"; else echo "escritura=no"; fi
if touch /etc/paso-lab 2>/dev/null; then echo "etc=escribible"; else echo "etc=no"; fi
EOF
printf '#!/bin/sh\necho "este paso falla"\nexit 3\n' >"${WD}/falla.sh"
chmod 0755 "${WD}/paso.sh" "${WD}/falla.sh"

# El archivo de entorno del proyecto, con sus secretos: solo de root.
install -d -m 0755 /etc/lab-systemd
install -m 0600 /dev/null /etc/lab-systemd/env
echo 'LAB_SECRETO=valor-que-el-agent-no-ve' >/etc/lab-systemd/env

# Lo del agent que un paso no tiene que poder leer.
install -d -m 0755 /etc/status-sv /etc/status-sv/keys
install -o statussv -g statussv -m 0400 /dev/null /etc/status-sv/keys/lab
install -d -o statussv -g statussv -m 0700 /var/lib/status-sv
install -o statussv -g statussv -m 0600 /dev/null /var/lib/status-sv/agent.json

# El servicio del proyecto y uno que no es suyo.
for u in lab-gunicorn lab-otra; do
  cat >/etc/systemd/system/${u}.service <<'EOF'
[Service]
User=labapp
ExecStart=/usr/bin/sleep infinity
EOF
done

# La declaración que lee el ayudante: la escribe root, y de ahí sale lo que
# corre. El ayudante de verdad lee el project.yaml con el parser del agent.
cat >/etc/status-sv/lab-ayudante.conf <<EOF
PROYECTO=lab
RUN_AS=labapp
WORKDIR=${WD}
ENV_FILE=/etc/lab-systemd/env
UNITS=(lab-gunicorn.service)
PASO_build_0=(${WD}/paso.sh)
TIEMPO_build_0=30
PASO_build_1=(${WD}/falla.sh)
TIEMPO_build_1=30
PASO_pre_0=(/usr/bin/sleep 60)
TIEMPO_pre_0=3
PASO_post_0=(/usr/bin/sleep 120)
TIEMPO_post_0=300
EOF
chmod 0600 /etc/status-sv/lab-ayudante.conf

install -m 0755 "${DIR}/sonda-systemd.sh" /usr/local/bin/sonda-systemd.sh
install -m 0755 "${DIR}/ayudante-falso.sh" /usr/local/bin/ayudante-falso.sh
install -m 0755 "${DIR}/cliente-ayudante.py" /usr/local/bin/cliente-ayudante.py

# El ayudante: se activa por el socket, que es root:statussv 0660, y corre como
# root sin capacidades, sin red y con el sistema de archivos en solo lectura.
cat >/etc/systemd/system/statussv-lab-ayudante.socket <<EOF
[Socket]
ListenStream=${SOCKET}
SocketUser=root
SocketGroup=statussv
SocketMode=0660
Accept=yes
EOF
cat >/etc/systemd/system/statussv-lab-ayudante@.service <<'EOF'
[Service]
ExecStart=/usr/local/bin/ayudante-falso.sh
StandardInput=socket
StandardOutput=socket
StandardError=journal
CapabilityBoundingSet=
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
PrivateNetwork=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_UNIX
RestrictNamespaces=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallArchitectures=native
SystemCallFilter=@system-service
EOF

# La unidad real del agent, con dos cambios: qué ejecuta y que corre una sola vez.
sed -e 's|^ExecStart=.*|ExecStart=/usr/local/bin/sonda-systemd.sh|' \
    -e 's|^Type=simple|Type=oneshot|' \
    -e '/^Restart=/d' -e '/^RestartSec=/d' \
    "${DIR}/../systemd/statussv-agent.service" >"${UNIT}"
mkdir -p "${DROPIN_DIR}"
printf '[Service]\nTimeoutStartSec=300\n' >"${DROPIN_DIR}/00-laboratorio.conf"
# Lo que deja el instalador con --enable-deploy (ADR-015).
cat >"${DROPIN_DIR}/20-deploy.conf" <<EOF
[Service]
Environment=STATUSSV_DEPLOY_ENABLED=true
ReadWritePaths=-${WD}
UMask=0002
MemoryMax=512M
TasksMax=256
EOF

systemctl daemon-reload
systemctl start lab-gunicorn lab-otra statussv-lab-ayudante.socket || { rojo "no arrancó la preparación"; exit 1; }

resultado=0

paso "el socket del ayudante"
ls -l "${SOCKET}"
# Otro usuario del servidor no llega al ayudante: el socket es del grupo statussv.
if runuser -u labapp -- python3 /usr/local/bin/cliente-ayudante.py "${SOCKET}" "restart_units lab" >/dev/null 2>&1; then
  rojo "FALLA otro usuario pudo pedirle un reinicio al ayudante"; resultado=1
else
  verde "OK    otro usuario no puede conectarse al ayudante"
fi

paso "la sonda, adentro de la unidad del agent"
desde="$(date '+%Y-%m-%d %H:%M:%S')"
systemctl reset-failed ${NAME} 2>/dev/null
if systemctl start ${NAME} >/dev/null 2>&1; then rc=0; else rc=1; fi
sleep 1
journalctl -u ${NAME} --since "${desde}" -o cat --no-pager 2>/dev/null | grep -Ev '^(Starting|Started|Finished|Stopped|Deactivated|Consumed|statussv-sonda)'
if [ "${rc}" -eq 0 ]; then verde "la sonda: todo lo que el motor necesita anda"; else rojo "la sonda: algo no dio lo esperado"; resultado=1; fi

# Lo que dejó el ayudante en el diario, por si algo falló.
journalctl -u 'statussv-lab-ayudante@*' --since "${desde}" -o cat --no-pager 2>/dev/null | grep -Ev '^(Starting|Started|Finished|Stopped|Deactivated|Consumed|statussv-lab-ayudante)' | sed 's/^/ayudante: /' | head -20

# --- La alternativa descartada: una regla de polkit por unidad ---
#
# Solo se mide. polkit entiende reglas en JavaScript desde la versión 0.106;
# Ubuntu 22.04 trae 0.105, que solo sabe dar manage-units entero.
paso "la regla de polkit por unidad (solo se mide)"
if [ -d /etc/polkit-1/rules.d ] && pkaction --version >/dev/null 2>&1; then
  cat >"${REGLA}" <<'EOF'
polkit.addRule(function(action, subject) {
    if (action.id == "org.freedesktop.systemd1.manage-units" && subject.user == "statussv") {
        if (action.lookup("unit") == "lab-gunicorn.service" && action.lookup("verb") == "restart") {
            return polkit.Result.YES;
        }
        return polkit.Result.NO;
    }
});
EOF
  sleep 1
  mide() { # $1 = qué, el resto = comando
    local que="$1"; shift
    if runuser -u statussv -- "$@" >/dev/null 2>&1; then echo "INFO  polkit: ${que}: permitido"; else echo "INFO  polkit: ${que}: rechazado"; fi
  }
  mide "reiniciar la unidad nombrada en la regla" systemctl --no-ask-password restart lab-gunicorn.service
  mide "reiniciar otra unidad" systemctl --no-ask-password restart lab-otra.service
  mide "arrancar una unidad transitoria" systemd-run --no-ask-password --quiet --wait --collect /usr/bin/true
  rm -f "${REGLA}"
else
  echo "INFO  polkit: sin reglas en JavaScript en esta máquina: la regla por unidad no existe"
fi

exit "${resultado}"
