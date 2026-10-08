#!/bin/bash
# Laboratorio del deploy de systemd (ADR-019): corre la unidad ENDURECIDA DE
# VERDAD del agent bajo systemd, con sonda-systemd.sh en lugar del agent, y
# prueba el reparto que usa el motor para un proyecto systemd: el agent no
# reinicia ni ejecuta nada del proyecto; se lo pide al ayudante root, por un
# socket unix, con operaciones fijas.
#
# El ayudante es el de verdad (statussv-agent unit-helper), con sus unidades
# de deploy/systemd tal cual se instalan.
#
# Necesita systemd como PID 1, python3 y el binario del agent para Linux. No
# necesita Docker. Se corre como root en una máquina descartable: el runner
# del CI (job "laboratorio") o WSL. Crea y borra lo suyo.
#
#   sudo AGENT_BIN=/ruta/a/statussv-agent ./deploy/laboratorio/systemd.sh
set -uo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
NAME=statussv-sonda
UNIT=/etc/systemd/system/${NAME}.service
DROPIN_DIR=/etc/systemd/system/${NAME}.service.d
WD=/srv/lab-systemd
ORIGEN=/srv/lab-systemd-origen.git
SOCKET=/run/status-sv/unit-helper.sock
REGLA=/etc/polkit-1/rules.d/49-statussv-laboratorio.rules
MARCA_USUARIO=/tmp/.lab-systemd-creo-statussv

rojo()  { printf '\033[31m%s\033[0m\n' "$*" >&2; }
verde() { printf '\033[32m%s\033[0m\n' "$*"; }
paso()  { printf '\n== %s ==\n' "$*"; }

[ "$(id -u)" -eq 0 ] || { rojo "hay que correrlo como root"; exit 2; }
for p in systemctl systemd-run journalctl python3 runuser git; do
  command -v "$p" >/dev/null || { rojo "falta ${p}"; exit 2; }
done
[ -x "${AGENT_BIN:-}" ] || { rojo "AGENT_BIN tiene que apuntar al binario del agent para Linux"; exit 2; }

limpiar() {
  paso "limpieza"
  systemctl disable --now statussv-agent statussv-unit-helper.socket >/dev/null 2>&1
  systemctl stop ${NAME} lab-gunicorn lab-otra 2>/dev/null
  systemctl disable lab-gunicorn lab-otra 2>/dev/null
  systemctl stop 'statussv-unit-helper@*.service' 'statussv-step-*.service' 2>/dev/null
  systemctl reset-failed ${NAME} 'statussv-unit-helper@*.service' 'statussv-step-*.service' 2>/dev/null
  rm -rf "${UNIT}" "${DROPIN_DIR}" "${REGLA}" \
    /etc/systemd/system/lab-gunicorn.service /etc/systemd/system/lab-otra.service \
    /etc/systemd/system/statussv-unit-helper.socket /etc/systemd/system/statussv-unit-helper@.service \
    /etc/systemd/system/statussv-agent.service /etc/systemd/system/statussv-agent.service.d
  systemctl daemon-reload
  rm -rf "${WD}" "${ORIGEN}" /etc/lab-systemd /etc/status-sv /var/lib/status-sv /run/status-sv /etc/paso-lab \
    /usr/local/bin/sonda-systemd.sh /usr/local/bin/cliente-ayudante.py /usr/local/bin/statussv-agent
  userdel labapp 2>/dev/null
  userdel labotro 2>/dev/null
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
# Otro usuario que está en el grupo del agent: llega al socket, y no es el agent.
useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --groups statussv labotro

# El proyecto es un repositorio de git, clonado por root como en un servidor.
TMP="$(mktemp -d)"
G=(git -c user.name=laboratorio -c user.email=lab@status-sv.invalid -c init.defaultBranch=main)
"${G[@]}" init -q "${TMP}"
cat >"${TMP}/paso.sh" <<'EOF'
#!/bin/sh
# Lo que haría un `pip install`: código del repositorio corriendo en el servidor.
echo "usuario=$(id -un)"
echo "directorio=$(pwd)"
echo "argumentos=$*"
if [ -n "${LAB_SECRETO:-}" ]; then echo "entorno=presente"; else echo "entorno=ausente"; fi
echo "cgroup=$(cut -d: -f3 /proc/self/cgroup)"
if cat /var/lib/status-sv/agent.json >/dev/null 2>&1; then echo "credencial=legible"; else echo "credencial=no"; fi
if cat /etc/status-sv/keys/lab >/dev/null 2>&1; then echo "llave=legible"; else echo "llave=no"; fi
if touch salida/construido.txt 2>/dev/null; then echo "escritura=ok"; else echo "escritura=no"; fi
if touch /etc/paso-lab 2>/dev/null; then echo "etc=escribible"; else echo "etc=no"; fi
EOF
printf '#!/bin/sh\necho "este paso falla"\nexit 3\n' >"${TMP}/falla.sh"
chmod 0755 "${TMP}/paso.sh" "${TMP}/falla.sh"
"${G[@]}" -C "${TMP}" add -A && "${G[@]}" -C "${TMP}" commit -qm v1
rm -rf "${ORIGEN}" "${WD}"
git clone -q --bare "${TMP}" "${ORIGEN}"
rm -rf "${TMP}"
git clone -q "file://${ORIGEN}" "${WD}"
# El origen queda del usuario del agent, que es quien lo va a traer: git se
# niega a servir, por file://, un repositorio que es de otro dueño. En un
# servidor el origen está en la red y esto no existe.
chown -R statussv "${ORIGEN}"

# Los permisos del runbook: el workdir sigue siendo de root, y el agent
# escribe por el grupo statussv en .git y en los directorios versionados. El
# usuario del proyecto escribe solo en lo suyo.
chgrp -R statussv "${WD}/.git"
chmod -R g+rwX "${WD}/.git"
find "${WD}/.git" -type d -exec chmod g+s {} +
chgrp statussv "${WD}" && chmod g+rwxs "${WD}"
install -d -o labapp -g labapp -m 0755 "${WD}/salida"

# El archivo de entorno del proyecto, con sus secretos: solo de root.
install -d -m 0755 /etc/lab-systemd
install -m 0600 /dev/null /etc/lab-systemd/env
echo 'LAB_SECRETO=valor-que-el-agent-no-ve' >/etc/lab-systemd/env

# Lo del agent que un paso no tiene que poder leer: su credencial (la de un
# agent ya enrolado contra un control plane que no existe) y una deploy key.
install -d -m 0755 /etc/status-sv /etc/status-sv/keys
install -o statussv -g statussv -m 0400 /dev/null /etc/status-sv/keys/lab
install -m 0644 /dev/null /etc/status-sv/known_hosts
install -d -o statussv -g statussv -m 0700 /var/lib/status-sv
install -o statussv -g statussv -m 0600 /dev/null /var/lib/status-sv/agent.json
cat >/var/lib/status-sv/agent.json <<'EOF'
{"server_url":"http://127.0.0.1:9","server_id":"01924f9c-0000-7000-8000-0000000000aa","server_name":"lab","credential":"svagt_credencial-del-laboratorio","enrolled_at":"2026-10-02T12:00:00Z"}
EOF

# El servicio del proyecto y uno que no es suyo.
for u in lab-gunicorn lab-otra; do
  cat >/etc/systemd/system/${u}.service <<'EOF'
[Service]
User=labapp
ExecStart=/usr/bin/sleep infinity

# Para probar que el ayudante habilita solo las del proyecto (enable_units).
[Install]
WantedBy=multi-user.target
EOF
done

# Los proyectos declarados, como los deja una persona: de root, legibles por
# el grupo del agent. De ahí saca el ayudante lo que corre.
install -d -o root -g statussv -m 0750 /etc/status-sv/projects.d
cat >/etc/status-sv/projects.d/lab.yaml <<EOF
name: lab
environment: staging
repo: file://${ORIGEN}
branch: main
runtime: systemd
workdir: ${WD}
units:
  - lab-gunicorn.service
run_as: labapp
env_file: /etc/lab-systemd/env
deploy:
  build:
    - command: ["./paso.sh", "un argumento", "--con=igual"]
      timeout_seconds: 30
    - command: ["./falla.sh"]
      timeout_seconds: 30
  pre:
    - command: ["/usr/bin/sleep", "60"]
      timeout_seconds: 3
  post:
    - command: ["/usr/bin/sleep", "120"]
      timeout_seconds: 300
EOF
# Un proyecto que no se puede desplegar: su usuario y su unidad no existen.
cat >/etc/status-sv/projects.d/roto.yaml <<EOF
name: roto
environment: staging
repo: github.com/status-sv/laboratorio
branch: main
runtime: systemd
workdir: ${WD}
units:
  - lab-no-existe.service
run_as: nadie-con-este-nombre
deploy:
  build:
    - command: ["./paso.sh"]
EOF
# Y uno de Docker, que al ayudante no le corresponde.
cat >/etc/status-sv/projects.d/contenedores.yaml <<EOF
name: contenedores
environment: staging
repo: github.com/status-sv/laboratorio
branch: main
runtime: docker
compose_file: compose.yml
workdir: ${WD}
EOF
chown root:statussv /etc/status-sv/projects.d/*.yaml
chmod 0640 /etc/status-sv/projects.d/*.yaml

install -m 0755 "${DIR}/sonda-systemd.sh" /usr/local/bin/sonda-systemd.sh
install -m 0755 "${DIR}/cliente-ayudante.py" /usr/local/bin/cliente-ayudante.py
install -m 0755 "${AGENT_BIN}" /usr/local/bin/statussv-agent

# El ayudante, con sus unidades tal cual se instalan en un servidor.
install -m 0644 "${DIR}/../systemd/statussv-unit-helper.socket" /etc/systemd/system/statussv-unit-helper.socket
install -m 0644 "${DIR}/../systemd/statussv-unit-helper@.service" /etc/systemd/system/statussv-unit-helper@.service

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
systemctl start lab-gunicorn lab-otra statussv-unit-helper.socket || { rojo "no arrancó la preparación"; exit 1; }

resultado=0

paso "el socket del ayudante"
ls -l "${SOCKET}"
PEDIDO='{"op":"restart_units","project":"lab"}'
# Otro usuario del servidor no llega al ayudante: el socket es del grupo statussv.
if runuser -u labapp -- python3 /usr/local/bin/cliente-ayudante.py "${SOCKET}" "${PEDIDO}" >/dev/null 2>&1; then
  rojo "FALLA otro usuario pudo pedirle un reinicio al ayudante"; resultado=1
else
  verde "OK    un usuario que no es del grupo no puede conectarse al ayudante"
fi
# Y uno del grupo que no es el agent llega al socket, pero el ayudante lo rechaza.
respuesta="$(runuser -u labotro -- python3 /usr/local/bin/cliente-ayudante.py "${SOCKET}" "${PEDIDO}" 2>&1)"
case "${respuesta}" in
  *'"code":"forbidden"'*) verde "OK    un usuario del grupo que no es el agent recibe forbidden" ;;
  *) rojo "FALLA un usuario del grupo que no es el agent recibió: ${respuesta}"; resultado=1 ;;
esac

# El ayudante corre como root lo que diga el project.yaml: si el grupo del
# agent pudiera escribirlo, el agent podría declararse sus propios pasos. Con
# esos permisos el ayudante no lo usa.
chmod 0660 /etc/status-sv/projects.d/lab.yaml
respuesta="$(python3 /usr/local/bin/cliente-ayudante.py "${SOCKET}" '{"op":"check","project":"lab"}' 2>&1)"
chmod 0640 /etc/status-sv/projects.d/lab.yaml
case "${respuesta}" in
  *'"code":"not_declared"'*) verde "OK    un project.yaml que el grupo puede escribir no se usa" ;;
  *) rojo "FALLA con el project.yaml escribible por el grupo contestó: ${respuesta}"; resultado=1 ;;
esac

paso "la sonda, adentro de la unidad del agent"
desde="$(date '+%Y-%m-%d %H:%M:%S')"
systemctl reset-failed ${NAME} 2>/dev/null
if systemctl start ${NAME} >/dev/null 2>&1; then rc=0; else rc=1; fi
sleep 1
journalctl -u ${NAME} --since "${desde}" -o cat --no-pager 2>/dev/null | grep -Ev '^(Starting|Started|Finished|Stopped|Deactivated|Consumed|statussv-sonda)'
if [ "${rc}" -eq 0 ]; then verde "la sonda: todo lo que el motor necesita anda"; else rojo "la sonda: algo no dio lo esperado"; resultado=1; fi

# Lo que dejó el ayudante en el diario.
echo
journalctl -u 'statussv-unit-helper@*' --since "${desde}" -o cat --no-pager 2>/dev/null \
  | grep -E 'componente=unit-helper' | sed 's/^/ayudante: /' | tail -25

# --- El instalador ---
#
# install-agent.sh --enable-deploy con un proyecto de systemd declarado: tiene
# que instalar el ayudante, sin pedir Docker, dejar el agent andando con el
# deploy prendido, y deploy-check, corrido como statussv, tiene que decir que
# el proyecto se puede desplegar. --disable-deploy saca el ayudante.
paso "el instalador con un proyecto de systemd"
# Los proyectos que estaban para probar rechazos se sacan: uno es de Docker,
# y con él el instalador pediría Docker.
rm -f /etc/status-sv/projects.d/roto.yaml /etc/status-sv/projects.d/contenedores.yaml
# El ayudante que se instaló a mano más arriba se saca: lo tiene que dejar el
# instalador.
systemctl disable --now statussv-unit-helper.socket >/dev/null 2>&1
rm -f /etc/systemd/system/statussv-unit-helper.socket /etc/systemd/system/statussv-unit-helper@.service
systemctl daemon-reload

instalador=0
dropin=/etc/systemd/system/statussv-agent.service.d/20-deploy.conf
if salida="$(STATUSSV_DEPLOY_ALLOW_FILE_REPOS=true bash "${DIR}/../install-agent.sh" --enable-deploy "${AGENT_BIN}" 2>&1)"; then
  printf '%s\n' "${salida}" | sed -n '/==> deploy/,$p'
  grep -qx "ReadWritePaths=-${WD}" "${dropin}" || { rojo "FALLA el drop-in no tiene el workdir"; instalador=1; }
  systemctl is-active --quiet statussv-unit-helper.socket || { rojo "FALLA el socket del ayudante no quedó activo"; instalador=1; }
  systemctl is-enabled --quiet statussv-unit-helper.socket || { rojo "FALLA el socket del ayudante no quedó habilitado"; instalador=1; }
  grep -q 'el ayudante responde' <<<"${salida}" || { rojo "FALLA deploy-check no habló con el ayudante"; instalador=1; }
  grep -q 'lab se puede desplegar' <<<"${salida}" || { rojo "FALLA deploy-check no aprobó el proyecto"; instalador=1; }
  systemctl is-active --quiet statussv-agent || { rojo "FALLA el agent no quedó andando"; instalador=1; }

  bash "${DIR}/../install-agent.sh" --disable-deploy "${AGENT_BIN}" >/dev/null 2>&1
  [ ! -e "${dropin}" ] || { rojo "FALLA --disable-deploy no sacó el drop-in"; instalador=1; }
  [ ! -e /etc/systemd/system/statussv-unit-helper.socket ] || { rojo "FALLA --disable-deploy no sacó el ayudante"; instalador=1; }
  systemctl is-active --quiet statussv-unit-helper.socket && { rojo "FALLA el socket del ayudante siguió activo"; instalador=1; }
else
  printf '%s\n' "${salida}"
  rojo "FALLA el instalador falló"
  instalador=1
fi
if [ "${instalador}" -eq 0 ]; then
  verde "el instalador: instala el ayudante, aprueba el proyecto y lo saca al apagar el deploy"
else
  resultado=1
fi

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
