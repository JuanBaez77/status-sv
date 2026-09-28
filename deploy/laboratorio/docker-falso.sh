#!/bin/sh
# Socket de Docker simulado para el laboratorio: lee el pedido y contesta 200.
while IFS= read -r line; do
  [ "$(printf '%s' "$line" | tr -d '\r')" = "" ] && break
done
printf 'HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 3\r\n\r\nOK\n'
