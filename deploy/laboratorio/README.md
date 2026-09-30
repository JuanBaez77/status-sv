# Laboratorio de la unidad del agent

Sirve para contestar una sola pregunta, sin tocar ningún VPS: **¿qué puede hacer el agent bajo el
endurecimiento real de [`../systemd/statussv-agent.service`](../systemd/statussv-agent.service)?**

Corre esa misma unidad bajo systemd de verdad, cambiando solo dos cosas (qué ejecuta y que corra
una vez), y en su lugar pone una sonda que prueba lo que el agent necesita y lo que no tiene que
poder. Hay que correrlo **antes de tocar el endurecimiento**, que es lo que pasó en el Sprint 2
(ADR-011) y va a volver a pasar en el Sprint 4 con el motor de deploy.

## Dónde correrlo

Donde systemd sea PID 1 y no sea un servidor de verdad:

- **WSL con `systemd=true`** en `/etc/wsl.conf` (`systemctl is-system-running` tiene que contestar
  algo distinto de `offline`).
- **Un contenedor privilegiado con systemd**: `ubuntu:24.04` con `systemd systemd-sysv dbus`,
  `systemctl mask systemd-logind getty.target console-getty systemd-udevd*`, `STOPSIGNAL SIGRTMIN+3`,
  `CMD ["/lib/systemd/systemd"]`, corrido con `--privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock`.

```bash
sudo ./laboratorio.sh   # los tres casos y la sonda
sudo ./limpieza.sh      # deja la máquina como estaba
```

## Qué prueba

`sonda.sh` corre **adentro** de la unidad endurecida:

| Comprobación | Por qué |
| --- | --- |
| Conectar a `/run/docker.sock` | Los contenedores (el socket lo simula `docker-falso.sh`, no hace falta Docker). |
| Leer `/sys/fs/cgroup/cpu.stat` | El consumo por contenedor y por servicio. |
| `ListUnits` por D-Bus | Los servicios de systemd. |
| `journalctl` de otra unidad | Los logs. |
| Escribir en `/etc` | **Tiene que fallar**: el agent no escribe fuera de su estado. |
| Escribir en `/var/lib/status-sv` | Su directorio de estado, que sí tiene que poder. |

Y tres escenarios de instalación:

- **A**: sin drop-in, como un VPS sin Docker. Todo funciona menos el socket, que es lo esperado.
- **B**: con `10-docker.conf` y el grupo `docker` existente, que es lo que deja el instalador.
- **C**: drop-in con un grupo que no existe. Tiene que **fallar** con `216/GROUP`: esa es la razón
  por la que el instalador escribe el drop-in solo si el grupo está.

## Límites

El socket de Docker es simulado: prueba el permiso y el sandbox, no la API de Docker. Lo que se
mide con un Docker de verdad (contenedores, eventos) va en los tests del adaptador.

## El deploy: `deploy.sh`

El motor de deploy del Sprint 4 ([ADR-015](../../docs/adr/ADR-015-motor-de-deploy-docker.md))
ejecuta `git` y `docker compose` como `statussv`, así que necesita un **Docker de verdad**, con el
plugin de Compose, además de systemd. Corre en cada PR, en el job `laboratorio` del CI, que tiene
las dos cosas; en local sirve una máquina descartable con systemd y Docker (WSL con Docker
instalado adentro, no el de Docker Desktop).

```bash
sudo ./deploy.sh   # prepara, corre los tres casos y limpia lo suyo al salir
```

Arma un proyecto Compose mínimo con dos commits en un repo `file://`, lo clona como root en
`/srv/lab-proyecto` con los permisos del runbook (dueño root, grupo `statussv` con escritura y
setgid en `.git` y en los directorios versionados), planta un hook `post-checkout` y genera una
deploy key. Después corre la unidad real con `sonda-deploy.sh`:

| Caso | Drop-ins | Qué se espera |
| --- | --- | --- |
| A | `10-docker.conf` | El `workdir` es de solo lectura y el `fetch` falla |
| C | más `20-deploy.conf` sin subir los techos (64M, 64 tareas) | Solo se mide |
| B | más `20-deploy.conf` completo | Todo lo que sigue anda |

En el caso B, la sonda comprueba: `git` sin `safe.directory` se niega (el `workdir` es de root) y
con él hace `fetch`, `merge-base --is-ancestor`, `status` limpio y `checkout -B`; el archivo nuevo
queda legible por todos (`UMask=0002`), el hook no corre, `ssh` negocia con github.com usando la
deploy key; `docker compose` hace `build`, `run --rm` y `up -d --no-build`, el contenedor queda
`healthy`, el healthcheck HTTP responde y la API de Docker etiqueta la imagen para el rollback. No
puede escribir en `/etc` ni en el `workdir` de otro proyecto. Al final informa el pico de memoria
y de tareas de la unidad.
