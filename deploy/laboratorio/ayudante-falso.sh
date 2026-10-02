#!/bin/bash
# Ayudante root de mentira para el laboratorio de systemd (ADR-019).
#
# Hace de `statussv-agent unit-helper` mientras ese no existe: lo activa un
# socket unix, corre como root sin capacidades y atiende un pedido por
# conexión, que es un renglón:
#
#   run_step <proyecto> <fase> <n> <deploy>   corre el paso y espera
#   stop_step <proyecto> <fase> <n> <deploy>  lo para
#   restart_units <proyecto>                  reinicia las unidades declaradas
#
# Lo que corre no viene en el pedido: sale de un archivo que escribió root
# (acá, /etc/status-sv/lab-ayudante.conf; el de verdad lee el project.yaml).
# Contesta un renglón: "ok ..." o "error ...".
set -u

CONF=/etc/status-sv/lab-ayudante.conf
# shellcheck source=/dev/null
. "${CONF}" || { echo "error sin declaración"; exit 0; }

IFS=' ' read -r op proyecto fase n deploy resto || exit 0

[ -z "${resto:-}" ] || { echo "error pedido con parámetros de más"; exit 0; }
[ "${proyecto:-}" = "${PROYECTO}" ] || { echo "error proyecto no declarado"; exit 0; }

unidad_del_paso() {
  case "${fase:-}" in build|pre|post) ;; *) echo "error fase desconocida"; exit 0 ;; esac
  [[ "${n:-}" =~ ^[0-4]$ ]] || { echo "error número de paso inválido"; exit 0; }
  [[ "${deploy:-}" =~ ^[0-9a-f]{8}$ ]] || { echo "error deploy inválido"; exit 0; }
  unidad="statussv-step-${deploy}-${fase}-${n}"
}

case "${op:-}" in
  run_step)
    unidad_del_paso
    declare -p "PASO_${fase}_${n}" >/dev/null 2>&1 || { echo "error paso no declarado"; exit 0; }
    declare -n comando="PASO_${fase}_${n}"
    declare -n tiempo="TIEMPO_${fase}_${n}"

    systemd-run --quiet --wait --unit "${unidad}" --service-type=oneshot \
      -p User="${RUN_AS}" -p WorkingDirectory="${WORKDIR}" -p EnvironmentFile="${ENV_FILE}" \
      -p TimeoutStartSec="${tiempo}" -p NoNewPrivileges=yes -p PrivateTmp=yes -p ProtectSystem=full \
      -- "${comando[@]}" </dev/null >/dev/null 2>&1 &
    hijo=$!

    # Si quien pidió el paso se va, el paso se para: no queda corriendo sin un
    # deploy que lo espere.
    cortado=no
    while kill -0 "${hijo}" 2>/dev/null; do
      read -r -t 1 _
      if [ $? -le 128 ]; then
        cortado=si
        systemctl stop "${unidad}.service" 2>/dev/null
        break
      fi
    done
    wait "${hijo}"
    salio=$?

    # Con un servicio de una sola vez, systemd-run dice solo si el arranque
    # falló: el motivo y el código de salida se le preguntan a la unidad.
    resultado="$(systemctl show -p Result --value "${unidad}.service" 2>/dev/null)"
    codigo="$(systemctl show -p ExecMainStatus --value "${unidad}.service" 2>/dev/null)"
    systemctl reset-failed "${unidad}.service" 2>/dev/null
    if [ "${cortado}" = si ]; then
      # Nadie espera la respuesta.
      exit 0
    elif [ "${salio}" -eq 0 ]; then
      echo "ok done"
    else
      echo "error ${resultado:-failed} ${codigo:-?}"
    fi
    ;;
  stop_step)
    unidad_del_paso
    systemctl stop "${unidad}.service" 2>/dev/null
    echo "ok stopped"
    ;;
  restart_units)
    [ -z "${fase:-}" ] || { echo "error pedido con parámetros de más"; exit 0; }
    for u in "${UNITS[@]}"; do
      systemctl restart "${u}" || { echo "error restart_failed ${u}"; exit 0; }
    done
    echo "ok restarted"
    ;;
  *)
    echo "error operación desconocida"
    ;;
esac
