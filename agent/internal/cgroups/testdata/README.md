# Archivos de cgroup v2 de muestra

Copias reales de la jerarquía `/sys/fs/cgroup`, para probar la lectura sin depender de la máquina
que corre los tests.

| Directorio | De dónde sale |
| --- | --- |
| `wsl-ubuntu/` | WSL Ubuntu con systemd 255 y cgroup v2, el 2026-09-27. `cgroup.controllers` de la raíz y el cgroup de `dbus.service`, con `cpu.stat`, `memory.current` y `memory.stat` leídos en el mismo instante para que sean coherentes entre sí. |

Los cgroups de contenedores no están acá porque en esa máquina Docker corre en otra distro. Sus
rutas son las que documenta Docker para cada driver de cgroups (`/system.slice/docker-<id>.scope`
con systemd, `/docker/<id>` con cgroupfs) y los tests las arman con `fstest.MapFS`.
