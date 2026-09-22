# Muestras de `/proc` para los tests del colector

Cada carpeta es un `/proc` en miniatura: los tests la leen como si fuera el de la máquina.

| Carpeta | Origen | Para qué |
| --- | --- | --- |
| `vps-ubuntu/` | Armada a mano | El caso típico: Ubuntu 24.04 con 2 núcleos y 8 GB, Docker con una red propia, snaps, un NFS, un disco con un espacio en el nombre y el aislamiento de la unidad del agent (`PrivateTmp`, `ProtectHome`, `PrivateDevices`, `StateDirectory`). Los números son inventados pero coherentes: la línea `cpu` es la suma de `cpu0` y `cpu1`. |
| `docker-desktop/` | Capturada de un kernel real (6.6, WSL2) desde un contenedor con `--network host` | Que los parsers aguanten la salida real del kernel, con todas sus líneas. La tabla de montajes es la de un contenedor: la raíz es `overlay` y lo único en disco son los archivos que Docker monta desde el host. |
| `systemd-sandbox/` | Capturada de una unidad con **el mismo endurecimiento** que `deploy/systemd/statussv-agent.service`, bajo systemd 255 en un contenedor | Cómo ve los montajes el agent de verdad. Antes de capturarla se montaron discos en `/home`, `/home/backups` y `/mnt/datos respaldo`: con `ProtectHome=true`, systemd saca de la tabla los dos de `/home` en lugar de dejarlos tapados. |

La estructura de `vps-ubuntu/self/mountinfo` sigue la de `systemd-sandbox`: la raíz cuelga de un
montaje que no figura en la tabla, y `/tmp`, `/var/tmp` y `/var/lib/status-sv` son bind mounts del
disco raíz.

Ninguna muestra tiene datos de servidores de clientes.
