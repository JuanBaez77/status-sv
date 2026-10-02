#!/bin/bash
# Sonda del laboratorio de systemd: corre como ExecStart de la unidad real del
# agent y hace, como statussv y bajo el mismo endurecimiento, lo que el motor
# va a hacer para desplegar un proyecto systemd (ADR-019): pedirle al ayudante
# root que corra un paso declarado y que reinicie las unidades del proyecto.
# También prueba lo que NO tiene que poder.
#
# Cada renglón dice OK o FALLA contra lo esperado. INFO es una medición, no una
# comprobación. Sale con 1 si algo no dio lo esperado.
set -u

fallas=0
ok()    { printf 'OK    %s\n' "$*"; }
falla() { printf 'FALLA %s\n' "$*"; fallas=$((fallas + 1)); }
info()  { printf 'INFO  %s\n' "$*"; }

SOCKET=/run/status-sv/unit-helper.sock
DEPLOY=0199a1b2
pide()  { python3 /usr/local/bin/cliente-ayudante.py "${SOCKET}" "$1" 2>&1; }
diario() { journalctl --unit "$1" --output cat --no-pager 2>/dev/null; }
prop()  { systemctl show -p "$2" --value "$1" 2>/dev/null; }

printf 'identidad: %s\n' "$(id)"

# --- Lo que el agent no puede hacer solo ---

if systemctl --no-ask-password restart lab-gunicorn.service >/dev/null 2>&1; then
  falla "directo: el agent pudo reiniciar una unidad sin el ayudante"
else
  ok "directo: systemd le niega reiniciar una unidad"
fi
if systemd-run --no-ask-password --quiet --wait --collect /usr/bin/true >/dev/null 2>&1; then
  falla "directo: el agent pudo arrancar una unidad transitoria"
else
  ok "directo: systemd le niega arrancar una unidad transitoria"
fi

# --- Lo que sí puede leer ---

if [ "$(prop lab-gunicorn.service ActiveState)" = active ] && [ "$(prop lab-gunicorn.service SubState)" = running ]; then
  ok "lectura: ve el estado de la unidad (MainPID $(prop lab-gunicorn.service MainPID), NRestarts $(prop lab-gunicorn.service NRestarts))"
else
  falla "lectura: no ve la unidad del proyecto como active/running"
fi

# --- Un paso declarado ---

respuesta="$(pide "run_step lab build 0 ${DEPLOY}")"
if [ "${respuesta}" = "ok done" ]; then ok "paso: el ayudante lo corrió y terminó bien"; else falla "paso: ${respuesta}"; fi

salida="$(diario "statussv-step-${DEPLOY}-build-0.service")"
if [ -n "${salida}" ]; then ok "paso: el agent lee su salida del diario"; else falla "paso: el agent no pudo leer la salida del paso"; fi
espera() { # $1 = renglón esperado, $2 = qué prueba
  if grep -qx "$1" <<<"${salida}"; then ok "paso: $2"; else falla "paso: $2 (faltó \"$1\")"; fi
}
espera "usuario=labapp" "corre con el usuario del proyecto"
espera "directorio=/srv/lab-systemd" "corre parado en el workdir"
espera "entorno=presente" "recibe el archivo de entorno, que el agent no puede leer"
espera "credencial=no" "no puede leer la credencial del agent"
espera "llave=no" "no puede leer las deploy keys"
espera "escritura=ok" "escribe donde escribe su usuario"
espera "etc=no" "no puede escribir en /etc"
cg="$(grep '^cgroup=' <<<"${salida}" | cut -d= -f2)"
case "${cg}" in
  */statussv-step-*) ok "paso: corre en su propio cgroup, no en el del agent (${cg})" ;;
  *) falla "paso: corrió en ${cg:-un cgroup desconocido}" ;;
esac
if cat /etc/lab-systemd/env >/dev/null 2>&1; then
  falla "entorno: el agent pudo leer el archivo de entorno del proyecto"
else
  ok "entorno: el agent no puede leer el archivo de entorno del proyecto"
fi

# --- Un paso que falla y uno que se pasa de tiempo ---

respuesta="$(pide "run_step lab build 1 ${DEPLOY}")"
case "${respuesta}" in
  "error exit-code 3") ok "paso que falla: informa el código de salida (3)" ;;
  *) falla "paso que falla: ${respuesta}" ;;
esac

inicio=$SECONDS
respuesta="$(pide "run_step lab pre 0 ${DEPLOY}")"
tardo=$((SECONDS - inicio))
case "${respuesta}" in
  "error timeout"*) if [ "${tardo}" -le 10 ]; then ok "tiempo máximo: el paso se corta a los ${tardo} s"; else falla "tiempo máximo: tardó ${tardo} s en cortarse"; fi ;;
  *) falla "tiempo máximo: ${respuesta} (${tardo} s)" ;;
esac

# --- Si el agent se va en medio de un paso, el paso se para ---

python3 /usr/local/bin/cliente-ayudante.py --cortar 2 "${SOCKET}" "run_step lab post 0 ${DEPLOY}" >/dev/null 2>&1
parado=no
for _ in $(seq 1 10); do
  estado="$(prop "statussv-step-${DEPLOY}-post-0.service" ActiveState)"
  if [ "${estado}" != active ] && [ "${estado}" != activating ]; then parado=si; break; fi
  sleep 1
done
if [ "${parado}" = si ]; then ok "corte: al cerrarse la conexión, el paso se paró"; else falla "corte: el paso siguió corriendo sin nadie que lo espere"; fi

# --- Reiniciar las unidades del proyecto ---

antes="$(prop lab-gunicorn.service MainPID)"
otra_antes="$(prop lab-otra.service MainPID)"
respuesta="$(pide "restart_units lab")"
despues="$(prop lab-gunicorn.service MainPID)"
if [ "${respuesta}" = "ok restarted" ] && [ -n "${despues}" ] && [ "${despues}" != "${antes}" ] && [ "${despues}" != 0 ]; then
  ok "reinicio: la unidad declarada cambió de proceso (${antes} → ${despues})"
else
  falla "reinicio: ${respuesta} (MainPID ${antes} → ${despues})"
fi
if [ "$(prop lab-otra.service MainPID)" = "${otra_antes}" ]; then
  ok "reinicio: la unidad que no es del proyecto no se tocó"
else
  falla "reinicio: se reinició una unidad que no es del proyecto"
fi

# --- Lo que el ayudante rechaza ---

rechaza() { # $1 = pedido, $2 = qué prueba
  local r
  r="$(pide "$1")"
  case "${r}" in
    error*) ok "rechazo: $2 (${r#error })" ;;
    *) falla "rechazo: $2: contestó \"${r}\"" ;;
  esac
}
rechaza "restart_units otro" "un proyecto que no está declarado"
rechaza "run_step lab build 4 ${DEPLOY}" "un paso que no está declarado"
rechaza "run_step lab shell 0 ${DEPLOY}" "una fase que no existe"
rechaza "run_step lab build 0 ${DEPLOY} /bin/sh" "un parámetro de más"
rechaza "restart lab-otra.service" "una operación que no existe"
rechaza "run_step lab build 0 ../../etc" "un deploy que no es un identificador"

# --- Consumo ---

cg="/sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup)"
for f in memory.peak pids.peak; do
  [ -r "${cg}/${f}" ] && info "cgroup del agent, ${f}: $(cat "${cg}/${f}")"
done

printf '%d fallas\n' "${fallas}"
[ "${fallas}" -eq 0 ]
