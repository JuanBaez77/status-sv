#!/usr/bin/env python3
"""Cliente del ayudante para el laboratorio de systemd (ADR-019).

Manda un pedido (un renglón de JSON) por el socket unix y muestra la respuesta. Con
--cortar N cierra la conexión a los N segundos sin esperar la respuesta, que es
lo que pasa si el agent muere en medio de un paso.

    cliente-ayudante.py /run/status-sv/unit-helper.sock '{"op":"restart_units","project":"lab"}'
"""

import socket
import sys
import time


def main() -> int:
    args = sys.argv[1:]
    cortar = None
    if len(args) >= 2 and args[0] == "--cortar":
        cortar = float(args[1])
        args = args[2:]
    if len(args) != 2:
        print("uso: cliente-ayudante.py [--cortar SEG] SOCKET PEDIDO", file=sys.stderr)
        return 2
    ruta, pedido = args

    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        s.connect(ruta)
    except OSError as e:
        print(f"no conecta: {e.strerror}")
        return 1
    s.sendall((pedido + "\n").encode())

    if cortar is not None:
        time.sleep(cortar)
        s.close()
        print("cortado")
        return 0

    s.settimeout(120)
    datos = b""
    try:
        while not datos.endswith(b"\n"):
            parte = s.recv(4096)
            if not parte:
                break
            datos += parte
    except socket.timeout:
        print("sin respuesta")
        return 1
    finally:
        s.close()
    respuesta = datos.decode(errors="replace").strip()
    print(respuesta)
    return 0 if '"ok":true' in respuesta else 1


if __name__ == "__main__":
    sys.exit(main())
