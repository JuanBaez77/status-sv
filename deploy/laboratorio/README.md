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
sudo ./deploy.sh                           # prepara, corre los casos A, C y B y limpia al salir
sudo AGENT_BIN=/tmp/statussv-agent ./deploy.sh   # además, el caso D con el instalador
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
| D | `install-agent.sh --enable-deploy` con el agent de verdad (si `AGENT_BIN` apunta a su binario) | El drop-in con el `workdir` declarado, el servicio andando con el deploy prendido, `deploy-check` aprobando el proyecto y `--disable-deploy` sacando el drop-in |

En el caso B, la sonda comprueba: `git` sin `safe.directory` se niega (el `workdir` es de root) y
con él hace `fetch`, `merge-base --is-ancestor`, `status` limpio y `checkout -B`; el archivo nuevo
queda legible por todos (`UMask=0002`), el hook no corre, `ssh` negocia con github.com usando la
deploy key; `docker compose` hace `build`, `run --rm` y `up -d --no-build`, el contenedor queda
`healthy`, el healthcheck HTTP responde y la API de Docker etiqueta la imagen para el rollback. No
puede escribir en `/etc` ni en el `workdir` de otro proyecto. Al final informa el pico de memoria
y de tareas de la unidad.

## El deploy de systemd: `systemd.sh`

Un proyecto systemd no tiene contenedor: instalar dependencias o migrar es ejecutar código del
repositorio en el servidor, y reiniciar su servicio necesita root. El agent no hace ninguna de las
dos cosas: se las pide a un ayudante root con operaciones fijas
([ADR-019](../../docs/adr/ADR-019-deploy-de-proyectos-systemd.md)). Este laboratorio prueba ese
reparto con el ayudante de verdad. Necesita systemd, `python3` y el binario del agent para Linux;
no necesita Docker.

```bash
sudo AGENT_BIN=/tmp/statussv-agent ./systemd.sh   # prepara, corre la sonda y limpia al salir
```

Arma un proyecto con su usuario (`labapp`), un servicio (`lab-gunicorn.service`), otro que no es
suyo (`lab-otra.service`) y un archivo de entorno que solo lee root, y lo declara en
`/etc/status-sv/projects.d` como lo haría una persona. Instala el ayudante
(`statussv-agent unit-helper`) con sus unidades de [`../systemd/`](../systemd/) tal cual: el socket
`root:statussv 0660` y el servicio sin capacidades, sin red y con el sistema de archivos en solo
lectura. Lo que corre sale del `project.yaml`, no del pedido.

`sonda-systemd.sh` corre adentro de la unidad real del agent y comprueba:

| Comprobación | Por qué |
| --- | --- |
| Reiniciar una unidad o arrancar una transitoria, directo | **Tiene que fallar**: el agent no tiene ese permiso |
| Leer el estado de la unidad (`ActiveState`, `MainPID`, `NRestarts`) | La verificación del deploy |
| `run_step`: el paso corre con el usuario del proyecto, en el `workdir`, con su archivo de entorno, en su propio cgroup | El código del proyecto no corre como hijo del agent |
| El paso no lee la credencial del agent ni las deploy keys, ni escribe en `/etc` | Lo que se gana con eso |
| El agent lee la salida del paso con `journalctl` | El avance del deploy |
| El agent no lee el archivo de entorno | Los secretos no pasan por él |
| Un paso que falla informa su código; uno que se pasa de tiempo se corta | El resultado del paso |
| Si la conexión se cierra, el paso se para | Un paso no queda corriendo sin un deploy que lo espere |
| `restart_units`: la unidad declarada cambia de proceso y la otra no | El reinicio |
| `check`: un proyecto listo, y uno cuyo usuario no existe | El chequeo previo del deploy |
| Proyecto sin declarar o de Docker; paso, fase u operación que no existen | **Tienen que rechazarse** |
| Un pedido que trae un comando o una unidad, o que no es JSON | **Tiene que rechazarse** sin tocar systemd |

Afuera de la unidad comprueba que un usuario que no es del grupo no llega al socket, que uno del
grupo que no es el agent recibe `forbidden`, y que el ayudante no usa un `project.yaml` que el grupo
puede escribir.

Al final **mide, sin comprobar**, la alternativa descartada: una regla de polkit que nombra la
unidad y el verbo. Con polkit 124 funciona (permite esa unidad y rechaza otra y las transitorias);
Ubuntu 22.04 trae polkit 0.105, que no entiende esas reglas.

Resultado del 2026-10-02 (systemd 255, polkit 124): las 34 comprobaciones de la sonda y las 3 de
afuera dan lo esperado. Los pasos corren en su propio cgroup, así que no cuentan contra la memoria
del agent.
