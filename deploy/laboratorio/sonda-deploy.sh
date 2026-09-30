#!/bin/bash
# Sonda del laboratorio de deploy: corre como ExecStart de la unidad real del
# agent y hace, como statussv y bajo el mismo endurecimiento, lo que el motor de
# deploy del Sprint 4 va a hacer (ADR-015): git con la deploy key, docker compose
# y la API de Docker. También prueba lo que NO tiene que poder.
#
# Cada renglón dice OK o FALLA contra lo esperado. INFO es una medición, no una
# comprobación. Sale con 1 si algo no dio lo esperado.
#
# Variables que pone deploy.sh en la unidad:
#   SONDA_WORKDIR   el workdir del proyecto de prueba
#   SONDA_ORIGEN    el repo de origen (file://)
#   SONDA_V2        el SHA que hay que desplegar
#   SONDA_ESPERA    "deploy" (con 20-deploy.conf) o "sin-deploy" (sin el drop-in)
set -u

fallas=0
ok()    { printf 'OK    %s\n' "$*"; }
falla() { printf 'FALLA %s\n' "$*"; fallas=$((fallas + 1)); }
info()  { printf 'INFO  %s\n' "$*"; }

WD="${SONDA_WORKDIR}"
ESTADO=/var/lib/status-sv

# El entorno de los hijos lo arma el agent desde cero (ADR-015): no hereda nada.
limpio() {
  env -i \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    HOME="${ESTADO}" DOCKER_CONFIG="${ESTADO}/docker" LANG=C \
    GIT_TERMINAL_PROMPT=0 GIT_CONFIG_NOSYSTEM=1 \
    GIT_SSH_COMMAND="ssh -F /dev/null -i /etc/status-sv/keys/lab -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/etc/status-sv/known_hosts" \
    "$@"
}

# git endurecido: la línea de comando pisa la configuración del repo.
GIT=(git -c core.hooksPath=/dev/null -c core.fsmonitor=false -c credential.helper=
     -c protocol.allow=never -c protocol.https.allow=always -c protocol.ssh.allow=always
     -c protocol.file.allow=always -c submodule.recurse=false -c "safe.directory=${WD}" -C "${WD}")

COMPOSE=(docker compose --project-directory "${WD}" -f "${WD}/compose.yml" -p lab)

printf 'identidad: %s\n' "$(id)"
printf 'umask:     %s\n' "$(umask)"
mkdir -p "${ESTADO}/docker"

# --- Escritura ---

if touch /etc/sonda 2>/dev/null; then falla "escritura: pudo escribir en /etc"; else ok "escritura: no puede en /etc"; fi
if touch /srv/otro-proyecto/sonda 2>/dev/null; then
  falla "escritura: pudo escribir en el workdir de otro proyecto"
else
  ok "escritura: no puede en el workdir de otro proyecto"
fi
if touch "${WD}/.sonda" 2>/dev/null; then
  rm -f "${WD}/.sonda"
  if [ "${SONDA_ESPERA}" = deploy ]; then ok "escritura: puede en su workdir"; else falla "escritura: pudo en el workdir sin el drop-in de deploy"; fi
else
  if [ "${SONDA_ESPERA}" = deploy ]; then falla "escritura: no puede en su workdir"; else ok "escritura: sin el drop-in, el workdir es de solo lectura"; fi
fi

if [ "${SONDA_ESPERA}" != deploy ]; then
  # Sin el drop-in, el fetch tiene que fallar: no puede escribir en .git.
  if limpio "${GIT[@]}" fetch --no-tags "${SONDA_ORIGEN}" +refs/heads/main:refs/remotes/statussv/main >/dev/null 2>&1; then
    falla "git: el fetch anduvo sin el drop-in de deploy"
  else
    ok "git: sin el drop-in, el fetch no puede escribir"
  fi
  printf '%d fallas\n' "${fallas}"
  [ "${fallas}" -eq 0 ]
  exit
fi

# --- git ---

# Sin safe.directory, git se niega: el workdir es de root.
if limpio git -C "${WD}" status >/dev/null 2>&1; then
  info "git: sin safe.directory también anduvo (el workdir no es de otro dueño)"
else
  ok "git: sin safe.directory se niega (el workdir es de root)"
fi

if salida="$(limpio "${GIT[@]}" fetch --no-tags "${SONDA_ORIGEN}" +refs/heads/main:refs/remotes/statussv/main 2>&1)"; then
  ok "git: fetch de la rama declarada"
else
  falla "git: fetch: ${salida}"
fi
if limpio "${GIT[@]}" merge-base --is-ancestor "${SONDA_V2}" refs/remotes/statussv/main 2>/dev/null; then
  ok "git: el commit pedido es ancestro de la punta"
else
  falla "git: merge-base --is-ancestor"
fi
if [ -z "$(limpio "${GIT[@]}" status --porcelain --untracked-files=no 2>&1)" ]; then
  ok "git: árbol limpio"
else
  falla "git: el árbol no está limpio: $(limpio "${GIT[@]}" status --porcelain --untracked-files=no 2>&1 | head -3)"
fi
if salida="$(limpio "${GIT[@]}" checkout -q -B main "${SONDA_V2}" 2>&1)"; then
  ok "git: checkout -B main <sha>"
else
  falla "git: checkout: ${salida}"
fi
if [ "$(limpio "${GIT[@]}" rev-parse HEAD 2>/dev/null)" = "${SONDA_V2}" ]; then
  ok "git: HEAD quedó en el commit pedido"
else
  falla "git: HEAD no quedó en el commit pedido"
fi
# El archivo nuevo tiene que quedar legible por todos: docker build lo copia
# con sus permisos, y la app adentro del contenedor puede no ser root.
modo="$(stat -c %a "${WD}/nuevo.html" 2>/dev/null || echo 'no existe')"
case "${modo}" in
  664|644) ok "git: el archivo nuevo quedó con modo ${modo}" ;;
  *) falla "git: el archivo nuevo quedó con modo ${modo} (la app del contenedor no lo podría leer)" ;;
esac
if [ -e /tmp/hook-corrio ]; then
  falla "git: corrió el hook post-checkout del workdir"
else
  ok "git: el hook post-checkout del workdir no corrió"
fi

# ssh con la deploy key: no hay llave autorizada, así que lo esperado es que
# GitHub la rechace. Eso prueba que ssh corre, resuelve, conecta y negocia bajo
# el sandbox.
if [ -s /etc/status-sv/known_hosts ]; then
  salida="$(limpio ssh -F /dev/null -i /etc/status-sv/keys/lab -o IdentitiesOnly=yes -o BatchMode=yes \
    -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/etc/status-sv/known_hosts -o ConnectTimeout=10 \
    -T git@github.com 2>&1)"
  case "${salida}" in
    *"Permission denied (publickey)"*) ok "ssh: conecta a github.com con la deploy key (rechazada, como se esperaba)" ;;
    *) falla "ssh: ${salida}" ;;
  esac
else
  info "ssh: sin known_hosts (sin red al preparar), se salta"
fi

# --- Docker ---

if salida="$(limpio docker version --format '{{.Server.Version}}' 2>&1)"; then
  ok "docker: la CLI habla con el demonio (${salida})"
else
  falla "docker: ${salida}"
fi
if salida="$(limpio docker compose version --short 2>&1)"; then
  ok "compose: ${salida}"
else
  falla "compose: ${salida}"
fi

inicio=$SECONDS
if salida="$(limpio "${COMPOSE[@]}" build 2>&1)"; then
  ok "compose: build ($((SECONDS - inicio)) s)"
else
  falla "compose: build: $(printf '%s' "${salida}" | tail -5)"
fi
if salida="$(limpio "${COMPOSE[@]}" run --rm --no-TTY web cat /www/nuevo.html 2>&1)"; then
  ok "compose: run --rm del paso declarado (${salida})"
else
  falla "compose: run: $(printf '%s' "${salida}" | tail -3)"
fi
if salida="$(limpio "${COMPOSE[@]}" up -d --no-build 2>&1)"; then
  ok "compose: up -d --no-build"
else
  falla "compose: up: $(printf '%s' "${salida}" | tail -5)"
fi

sano=""
for _ in $(seq 1 30); do
  sano="$(limpio docker inspect --format '{{.State.Health.Status}}' lab-web-1 2>/dev/null)"
  [ "${sano}" = healthy ] && break
  sleep 1
done
if [ "${sano}" = healthy ]; then ok "docker: el contenedor quedó healthy"; else falla "docker: el contenedor quedó ${sano:-sin estado}"; fi

if salida="$(curl -sS --max-time 5 http://127.0.0.1:18080/nuevo.html 2>&1)"; then
  ok "healthcheck HTTP: responde (${salida})"
else
  falla "healthcheck HTTP: ${salida}"
fi

# El rollback etiqueta por la API, con el cliente propio del agent; acá, curl.
imagen="$(curl -sS --unix-socket /run/docker.sock http://d/containers/lab-web-1/json 2>/dev/null \
  | grep -o '"Image":"sha256:[0-9a-f]*"' | head -1 | cut -d'"' -f4)"
codigo="$(curl -sS -o /dev/null -w '%{http_code}' --unix-socket /run/docker.sock -X POST \
  "http://d/images/${imagen}/tag?repo=statussv-rollback/lab-web&tag=sonda" 2>&1)"
if [ "${codigo}" = 201 ]; then
  ok "API de Docker: etiqueta la imagen para el rollback"
else
  falla "API de Docker: etiquetar devolvió ${codigo}"
fi

# --- Consumo ---

cg="/sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup)"
for f in memory.peak pids.peak memory.max pids.max; do
  [ -r "${cg}/${f}" ] && info "cgroup ${f}: $(cat "${cg}/${f}")"
done

printf '%d fallas\n' "${fallas}"
[ "${fallas}" -eq 0 ]
