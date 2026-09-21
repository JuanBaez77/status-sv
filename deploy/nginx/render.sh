#!/usr/bin/env bash
#
# Genera la configuración de nginx de status-sv a partir de una plantilla.
#
#   deploy/nginx/render.sh bootstrap status.ejemplo.com 127.0.0.1:8090
#   deploy/nginx/render.sh final     status.ejemplo.com 127.0.0.1:8090
#
# Escribe el resultado por la salida estándar. "bootstrap" es la configuración
# provisoria para pedir el primer certificado; "final" es la definitiva.
#
# Los parámetros se validan antes de reemplazarlos en la plantilla: a la
# configuración de nginx de un servidor de producción no llega texto libre.
#
set -euo pipefail

TEMPLATE_DIR="$(cd "$(dirname "$0")" && pwd)"
readonly TEMPLATE_DIR

error() { printf 'error: %s\n' "$*" >&2; exit 1; }

[ $# -eq 3 ] || error "uso: render.sh bootstrap|final DOMINIO HOST:PUERTO"

MODE="$1"
DOMAIN="$2"
UPSTREAM="$3"

case "${MODE}" in
  bootstrap) TEMPLATE="${TEMPLATE_DIR}/status-sv.bootstrap.conf.template" ;;
  final) TEMPLATE="${TEMPLATE_DIR}/status-sv.conf.template" ;;
  *) error "el modo tiene que ser bootstrap o final, no «${MODE}»" ;;
esac

# Un nombre de dominio: etiquetas de letras, números y guiones, separadas por
# puntos. Nada de espacios, punto y coma ni llaves, que romperían la sintaxis de
# nginx o permitirían inyectar directivas.
DOMAIN_RE='^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$'
[[ "${DOMAIN}" =~ ${DOMAIN_RE} ]] || error "dominio inválido: «${DOMAIN}»"

# El destino es siempre local: el control plane no se publica fuera de loopback.
UPSTREAM_RE='^127\.0\.0\.1:([0-9]{2,5})$'
[[ "${UPSTREAM}" =~ ${UPSTREAM_RE} ]] || error "el destino tiene que ser 127.0.0.1:PUERTO, no «${UPSTREAM}»"
PORT="${BASH_REMATCH[1]}"
[ "${PORT}" -ge 1024 ] && [ "${PORT}" -le 65535 ] || error "puerto fuera de rango: ${PORT}"

[ -f "${TEMPLATE}" ] || error "no se encontró la plantilla ${TEMPLATE}"

sed -e "s|__DOMAIN__|${DOMAIN}|g" -e "s|__UPSTREAM__|${UPSTREAM}|g" "${TEMPLATE}"
