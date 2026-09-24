#!/bin/bash
# Sonda del laboratorio: corre como ExecStart de la unidad real del agent y
# prueba las cuatro cosas que el Sprint 2 necesita, más una que NO tiene que poder.
set -u
ok()    { printf 'OK    %s\n' "$*"; }
falla() { printf 'FALLA %s\n' "$*"; }

printf 'identidad: %s\n' "$(id)"

# 1. Socket de Docker (acá lo simula socat, que responde 200 a cualquier cosa).
salida="$(curl -sS --max-time 5 --unix-socket /run/docker.sock http://localhost/_ping 2>&1)"
if [ $? -eq 0 ]; then
  ok "socket de Docker: conecta y responde"
else
  falla "socket de Docker: ${salida}"
fi

# 2. Consumo por cgroup v2.
if head -1 /sys/fs/cgroup/cpu.stat >/dev/null 2>&1; then
  ok "cgroup v2: $(head -1 /sys/fs/cgroup/cpu.stat)"
else
  falla "cgroup v2: no se pudo leer /sys/fs/cgroup/cpu.stat"
fi

# 3. Bus del sistema, que es por donde se leen los servicios de systemd.
if busctl --system --no-pager list >/dev/null 2>&1; then
  unidades="$(busctl --system --no-pager call org.freedesktop.systemd1 /org/freedesktop/systemd1 \
      org.freedesktop.systemd1.Manager ListUnits 2>&1 | head -c 60)"
  ok "D-Bus del sistema: ListUnits responde (${unidades}…)"
else
  falla "D-Bus del sistema: no se pudo hablar con org.freedesktop.systemd1"
fi

# 4. Diario, que es de donde salen los logs de un servicio.
if journalctl --unit dbus.service --lines 2 --output json --no-pager >/dev/null 2>&1; then
  ok "journalctl: lee el diario de otra unidad"
else
  falla "journalctl: no pudo leer el diario"
fi

# 5. Lo que NO tiene que poder: escribir fuera de su directorio de estado.
if touch /etc/sonda 2>/dev/null; then
  falla "endurecimiento: pudo escribir en /etc"
else
  ok "endurecimiento: no puede escribir en /etc"
fi
if touch /var/lib/status-sv/sonda 2>/dev/null; then
  ok "estado: puede escribir en /var/lib/status-sv"
else
  falla "estado: no puede escribir en su propio directorio"
fi
