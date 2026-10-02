#!/bin/bash
# Sonda del laboratorio de systemd: corre como ExecStart de la unidad real del
# agent y hace, como statussv y bajo el mismo endurecimiento, lo que el motor
# hace para desplegar un proyecto systemd (ADR-019): pedirle al ayudante root
# que corra un paso declarado y que reinicie las unidades del proyecto.
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
DEPLOY=0199a1b2-0000-7000-8000-00000000000a
pide()   { python3 /usr/local/bin/cliente-ayudante.py "${SOCKET}" "$1" 2>&1; }
paso()   { printf '{"op":"%s","project":"%s","phase":"%s","index":%s,"deploy":"%s"}' "$1" "$2" "$3" "$4" "${DEPLOY}"; }
unidad() { printf 'statussv-step-%s-%s-%s.service' "${DEPLOY}" "$1" "$2"; }
diario() { journalctl --unit "$1" --output cat --no-pager 2>/dev/null; }
prop()   { systemctl show -p "$2" --value "$1" 2>/dev/null; }

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

# --- El chequeo previo ---

respuesta="$(pide '{"op":"check","project":"lab"}')"
if [ "${respuesta}" = '{"ok":true}' ]; then ok "chequeo: el proyecto se puede desplegar"; else falla "chequeo: ${respuesta}"; fi
respuesta="$(pide '{"op":"check","project":"roto"}')"
case "${respuesta}" in
  *'"code":"not_ready"'*) ok "chequeo: un proyecto cuyo usuario no existe no está listo" ;;
  *) falla "chequeo de un proyecto roto: ${respuesta}" ;;
esac

# --- Un paso declarado ---

respuesta="$(pide "$(paso run_step lab build 0)")"
if [ "${respuesta}" = '{"ok":true}' ]; then ok "paso: el ayudante lo corrió y terminó bien"; else falla "paso: ${respuesta}"; fi

salida="$(diario "$(unidad build 0)")"
if [ -n "${salida}" ]; then ok "paso: el agent lee su salida del diario"; else falla "paso: el agent no pudo leer la salida del paso"; fi
espera() { # $1 = renglón esperado, $2 = qué prueba
  if grep -qxF "$1" <<<"${salida}"; then ok "paso: $2"; else falla "paso: $2 (faltó \"$1\")"; fi
}
espera "usuario=labapp" "corre con el usuario del proyecto"
espera "directorio=/srv/lab-systemd" "corre parado en el workdir"
espera "argumentos=un argumento --con=igual" "recibe los argumentos declarados, sin partirlos"
espera "entorno=presente" "recibe el archivo de entorno, que el agent no puede leer"
espera "credencial=no" "no puede leer la credencial del agent"
espera "llave=no" "no puede leer las deploy keys"
espera "escritura=ok" "escribe donde escribe su usuario"
espera "etc=no" "no puede escribir en /etc"
cg="$(grep '^cgroup=' <<<"${salida}" | cut -d= -f2)"
case "${cg}" in
  */statussv-step-*) ok "paso: corre en su propio cgroup, no en el del agent" ;;
  *) falla "paso: corrió en ${cg:-un cgroup desconocido}" ;;
esac
if cat /etc/lab-systemd/env >/dev/null 2>&1; then
  falla "entorno: el agent pudo leer el archivo de entorno del proyecto"
else
  ok "entorno: el agent no puede leer el archivo de entorno del proyecto"
fi

# --- Un paso que falla y uno que se pasa de tiempo ---

respuesta="$(pide "$(paso run_step lab build 1)")"
case "${respuesta}" in
  *'"code":"step_failed"'*'"exit_code":3'*) ok "paso que falla: informa el código de salida (3)" ;;
  *) falla "paso que falla: ${respuesta}" ;;
esac
# El mismo pedido otra vez: la unidad fallida de recién no lo traba.
respuesta="$(pide "$(paso run_step lab build 1)")"
case "${respuesta}" in
  *'"code":"step_failed"'*) ok "paso repetido: vuelve a correr" ;;
  *) falla "paso repetido: ${respuesta}" ;;
esac

inicio=$SECONDS
respuesta="$(pide "$(paso run_step lab pre 0)")"
tardo=$((SECONDS - inicio))
case "${respuesta}" in
  *'"code":"timeout"'*) if [ "${tardo}" -le 10 ]; then ok "tiempo máximo: el paso se corta a los ${tardo} s"; else falla "tiempo máximo: tardó ${tardo} s en cortarse"; fi ;;
  *) falla "tiempo máximo: ${respuesta} (${tardo} s)" ;;
esac

# --- Si el agent se va en medio de un paso, el paso se para ---

python3 /usr/local/bin/cliente-ayudante.py --cortar 2 "${SOCKET}" "$(paso run_step lab post 0)" >/dev/null 2>&1
parado=no
for _ in $(seq 1 10); do
  estado="$(prop "$(unidad post 0)" ActiveState)"
  if [ "${estado}" != active ] && [ "${estado}" != activating ]; then parado=si; break; fi
  sleep 1
done
if [ "${parado}" = si ]; then ok "corte: al cerrarse la conexión, el paso se paró"; else falla "corte: el paso siguió corriendo sin nadie que lo espere"; fi

# --- Parar un paso a pedido ---

respuesta="$(pide "$(paso stop_step lab post 0)")"
if [ "${respuesta}" = '{"ok":true}' ]; then ok "parar: un paso que ya no corre se para sin error"; else falla "parar: ${respuesta}"; fi

# --- Reiniciar las unidades del proyecto ---

antes="$(prop lab-gunicorn.service MainPID)"
otra_antes="$(prop lab-otra.service MainPID)"
respuesta="$(pide '{"op":"restart_units","project":"lab"}')"
despues="$(prop lab-gunicorn.service MainPID)"
if [ "${respuesta}" = '{"ok":true}' ] && [ -n "${despues}" ] && [ "${despues}" != "${antes}" ] && [ "${despues}" != 0 ]; then
  ok "reinicio: la unidad declarada cambió de proceso (${antes} → ${despues})"
else
  falla "reinicio: ${respuesta} (MainPID ${antes} → ${despues})"
fi
if [ "$(prop lab-otra.service MainPID)" = "${otra_antes}" ]; then
  ok "reinicio: la unidad que no es del proyecto no se tocó"
else
  falla "reinicio: se reinició una unidad que no es del proyecto"
fi

# --- Parar las unidades del proyecto: lo que deshace un primer deploy ---

respuesta="$(pide '{"op":"stop_units","project":"lab"}')"
if [ "${respuesta}" = '{"ok":true}' ] && [ "$(prop lab-gunicorn.service ActiveState)" = inactive ]; then
  ok "parada: la unidad declarada quedó parada"
else
  falla "parada: ${respuesta} (lab-gunicorn.service quedó $(prop lab-gunicorn.service ActiveState))"
fi
if [ "$(prop lab-otra.service MainPID)" = "${otra_antes}" ]; then
  ok "parada: la unidad que no es del proyecto no se tocó"
else
  falla "parada: se tocó una unidad que no es del proyecto"
fi
# Un reinicio la levanta de nuevo, que es como arranca un primer deploy.
respuesta="$(pide '{"op":"restart_units","project":"lab"}')"
if [ "${respuesta}" = '{"ok":true}' ] && [ "$(prop lab-gunicorn.service ActiveState)" = active ]; then
  ok "parada: un reinicio levanta de nuevo la unidad parada"
else
  falla "parada: después de reiniciar, ${respuesta} (lab-gunicorn.service quedó $(prop lab-gunicorn.service ActiveState))"
fi

# --- Lo que el ayudante rechaza ---

rechaza() { # $1 = pedido, $2 = código esperado, $3 = qué prueba
  local r
  r="$(pide "$1")"
  case "${r}" in
    *"\"code\":\"$2\""*) ok "rechazo: $3 ($2)" ;;
    *) falla "rechazo: $3: contestó ${r}" ;;
  esac
}
rechaza '{"op":"restart_units","project":"otro"}' not_declared "un proyecto que no está declarado"
rechaza '{"op":"restart_units","project":"contenedores"}' not_declared "un proyecto de Docker"
rechaza "$(paso run_step lab build 4)" no_such_step "un paso que no está declarado"
rechaza "$(paso run_step lab shell 0)" invalid_request "una fase que no existe"
rechaza '{"op":"run_step","project":"lab","phase":"build","index":0,"deploy":"'"${DEPLOY}"'","command":["/bin/sh"]}' invalid_request "un pedido que trae un comando"
rechaza '{"op":"restart_units","project":"lab","unit":"lab-otra.service"}' invalid_request "un pedido que trae una unidad"
rechaza '{"op":"stop_units","project":"lab","unit":"lab-otra.service"}' invalid_request "una parada que trae una unidad"
rechaza '{"op":"restart","project":"lab"}' invalid_request "una operación que no existe"
rechaza '{"op":"run_step","project":"lab","phase":"build","index":0,"deploy":"../../etc"}' invalid_request "un deploy que no es un identificador"
rechaza 'run_step lab build 0' invalid_request "un pedido que no es JSON"
if [ "$(prop lab-otra.service MainPID)" = "${otra_antes}" ]; then
  ok "rechazo: después de todo eso, la unidad ajena sigue igual"
else
  falla "rechazo: la unidad ajena cambió de proceso"
fi

# --- Consumo ---

cg="/sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup)"
for f in memory.peak pids.peak; do
  [ -r "${cg}/${f}" ] && info "cgroup del agent, ${f}: $(cat "${cg}/${f}")"
done

printf '%d fallas\n' "${fallas}"
[ "${fallas}" -eq 0 ]
