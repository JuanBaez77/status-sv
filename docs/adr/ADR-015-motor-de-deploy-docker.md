# ADR-015: Motor de deploy de proyectos Docker

- **Estado:** aceptada
- **Fecha:** 2026-09-30

## Contexto

El Sprint 4 abre la Fase 2: desplegar un proyecto Docker declarado, con healthcheck y rollback
automático, disparado desde el panel. Es la primera operación de escritura del agent, y choca con
varias cosas que hasta ahora eran de solo lectura:

- **La unidad del agent** tiene `ProtectSystem=strict` (no puede escribir en el `workdir` de un
  proyecto), `MemoryMax=64M` y `TasksMax=64`. Un `docker compose` o un `git` que el agent ejecute
  viven dentro de esos mismos límites.
- **El código de los proyectos es privado** y hoy se actualiza con las credenciales de root. El
  agent corre con su propio usuario y no puede usarlas.
- **[ADR-007](ADR-007-control-plane-en-servidor-de-cliente.md)** pide mudar el control plane antes
  de habilitar herramientas de escritura, y la mudanza ([ADR-014](ADR-014-control-plane-en-vps-propio.md))
  está postergada.
- **La regla 1** prohíbe ejecutar texto libre, y la regla 2 limita el agent a una lista cerrada de
  operaciones sobre proyectos declarados.

Las siete decisiones las tomó Juan el 2026-09-30, aceptando las recomendaciones del plan.

## Decisión

**1. El deploy se construye entero, pero cada agent lo trae apagado.**

- Lo prende una persona en el servidor, con `STATUSSV_DEPLOY_ENABLED=true` en el drop-in
  `20-deploy.conf` de la unidad. El instalador lo escribe solo si se le pide (`--enable-deploy`).
- Con el deploy apagado, el agent rechaza `deploy` y `rollback` con el código `disabled`: el
  control plane no puede prenderlo, ni aunque esté comprometido.
- Prenderlo en producción exige antes una de dos cosas: mudar el control plane (ADR-014) o
  enmendar ADR-007 aceptando el riesgo por escrito. Se decide al desplegar el sprint.

**2. El agent ejecuta `git` y `docker compose` con argumentos fijos, como `journalctl` en
[ADR-011](ADR-011-acceso-a-docker-y-journald.md).**

- `exec.Command` con el programa por ruta absoluta y los argumentos en una lista: nunca un
  intérprete de shell ni una cadena que alguien parsee. Cada valor que entra en un argumento viene
  del `project.yaml` validado o de un SHA de 40 caracteres hexadecimales.
- El entorno del proceso hijo no se hereda: se arma con `PATH` fijo, `HOME` y `DOCKER_CONFIG` en
  `/var/lib/status-sv`, `LANG=C`, `GIT_TERMINAL_PROMPT=0`, `GIT_CONFIG_NOSYSTEM=1` y el
  `GIT_SSH_COMMAND` del punto 3.
- Lo que el agent puede resolver por la API de Docker lo hace con su cliente propio (listar los
  contenedores del proyecto, leer sus imágenes, etiquetarlas), sin ejecutar nada.
- Reimplementar Compose no tiene sentido: resuelve dependencias, redes, volúmenes, build y
  recreación. El SDK tampoco: pesa más que la meta de 15 MB del binario.

**3. El código sale del repo declarado, con una deploy key por proyecto.**

- La URL se arma desde `repo`: `github.com/dueño/repo` se trae por SSH
  (`ssh://git@github.com/dueño/repo.git`) si el proyecto tiene llave, y por HTTPS si no la tiene
  (repos públicos). No se usa el `origin` del `workdir`: el agent no depende de cómo lo clonó alguien.
- La llave es de solo lectura, una por proyecto, en `/etc/status-sv/keys/<proyecto>`
  (`statussv:statussv 0400`), y los host keys conocidos en `/etc/status-sv/known_hosts`. Las pone
  una persona por SSH, como el `project.yaml`. `ssh` corre con `BatchMode=yes`,
  `IdentitiesOnly=yes`, `StrictHostKeyChecking=yes` y `-F /dev/null`.
- Se despliega **un SHA exacto**. Por defecto, la punta de la rama declarada después del
  `fetch`. Si se pide un commit en particular, tiene que ser ancestro de esa punta: lo que llega a
  la rama protegida ya pasó por un PR con CI verde. Consultar el estado del CI en GitHub queda para
  la Fase 3.
- `git` corre endurecido desde la línea de comando, que pisa la configuración del repo:
  `core.hooksPath=/dev/null` (ningún hook del `workdir` se ejecuta), `core.fsmonitor=false`,
  `credential.helper=` vacío, `protocol.allow=never` con solo `https` y `ssh` permitidos,
  `submodule.recurse=false` y `safe.directory` con el `workdir`.
- El árbol de trabajo tiene que estar limpio (`git status --porcelain --untracked-files=no`). Si
  alguien editó un archivo a mano en el servidor, el deploy no lo pisa: falla antes de empezar y
  lo explica.
- El checkout es `git checkout -B <rama> <sha>`: la rama local queda apuntando al commit
  desplegado, así un `git log` en el servidor dice la verdad.

**4. Pasos del deploy, en este orden.**

| Paso | Qué hace | Si falla |
| --- | --- | --- |
| `preflight` | `workdir`, repo, árbol limpio, archivo de Compose, llave, que no haya otro deploy | Nada que deshacer |
| `fetch` | `git fetch` de la rama y resolución del SHA | Nada que deshacer |
| `snapshot` | Anota el SHA actual y la imagen de cada servicio que corre, y la etiqueta para volver | Nada que deshacer |
| `checkout` | `git checkout -B <rama> <sha>` | Vuelve el código |
| `build` | `docker compose build` | Vuelve el código y las etiquetas de imagen |
| `pre` | Pasos declarados antes de levantar ([ADR-016](ADR-016-pasos-declarados-del-deploy.md)) | Ídem |
| `up` | `docker compose up -d --no-build` | Ídem, y `up -d --no-build` con lo anterior |
| `healthcheck` | Ventana de verificación (punto 6) | Ídem |
| `post` | Pasos declarados después de verificar | Ídem |

El `build` va antes que todo lo que toca lo que corre: mientras compila, el sitio sigue con la
versión anterior. `up` no usa `--remove-orphans`: un servicio que el commit nuevo ya no declara
queda andando, y lo saca una persona.

**5. El rollback vuelve a la imagen anterior, sin recompilar.**

- En `snapshot`, el agent lee por la API de Docker el ID de imagen de cada contenedor del proyecto
  y lo etiqueta `statussv-rollback/<proyecto>-<servicio>:<sha corto>`. Se guardan las de los
  últimos 3 deploys buenos; las más viejas se desetiquetan (no se borra ninguna imagen: eso lo
  decide el `prune` de quien administre el servidor).
- Volver atrás es: reetiquetar cada ID anotado con el nombre de imagen que usaba el contenedor,
  `git checkout -B <rama> <sha anterior>` y `docker compose up -d --no-build`. Compose ve que la
  imagen cambió y recrea con la anterior.
- Un rollback automático no vuelve a verificar con la ventana completa: si la versión anterior
  tampoco levanta, el deploy termina en `failed` y lo dice.
- El rollback manual (desde el panel) es un deploy hacia el último estado bueno anotado (SHA e
  imágenes), sin `build` ni pasos, con su propia ventana de verificación y su propio snapshot: si
  falla, vuelve a donde estaba.
- La base de datos **no** vuelve atrás: si un paso `pre` migró el esquema, el rollback solo
  devuelve el código y las imágenes. Lo advierte ADR-016.

**6. La verificación mira dos cosas durante hasta 120 s, cada 2 s.**

- Los contenedores del proyecto: ninguno en `restarting`, `dead` ni `exited` con código distinto
  de 0, y los que tienen healthcheck de Docker en `healthy`.
- El healthcheck HTTP declarado, con las mismas reglas que el pasivo (sin seguir redirecciones,
  verificando el certificado): tres respuestas seguidas con el código esperado.
- Pasa cuando las dos se cumplen tres veces seguidas. Sin healthcheck declarado, alcanza con los
  contenedores.

**7. Un deploy a la vez por agent, con diario en disco.**

- El agent ejecuta un solo `deploy` o `rollback` por vez: un build es pesado y los clientes
  comparten el servidor. Otro pedido mientras tanto se rechaza con `busy`. El control plane además
  impide dos deploys activos sobre el mismo proyecto.
- Cada deploy lleva un diario en `/var/lib/status-sv/deploys/<id>.json`, que se escribe en cada
  cambio de paso. Si el canal se corta, el deploy sigue y el diario se manda al reconectar.
- Si el **proceso** del agent se reinicia a mitad de un deploy, systemd mata a sus hijos (el
  `compose` en curso incluido). Al arrancar, el agent encuentra el diario sin terminar, vuelve
  atrás con el snapshot y lo informa como `failed`, con el motivo.
- Tiempos máximos: `fetch` 2 minutos, `build` 15, `up` 5, verificación 2, cada paso declarado lo
  que declare (hasta 30), y el deploy entero 45.

**8. El avance viaja en `deploy_event`, con la foto completa del deploy.**

- Cada mensaje trae el estado entero del deploy (pasos, resultado de cada uno, SHAs y los últimos
  renglones de salida de cada paso), no un evento suelto: si se pierde uno, el siguiente lo
  reemplaza. Lleva un número de secuencia y el control plane ignora uno más viejo que el guardado.
- La salida de cada paso pasa por el mismo filtro que los logs (sin secuencias de escape, secretos
  enmascarados, recortada después de enmascarar). Se guardan los últimos 20 renglones por paso, de
  hasta 256 caracteres.
- El `command` de `deploy` se contesta enseguida con `command_ack` (aceptado o rechazado); el
  resultado llega después, por `deploy_event`.

**9. La unidad gana lo justo, en un drop-in que escribe el instalador.**

`20-deploy.conf` lleva `STATUSSV_DEPLOY_ENABLED=true`, `ReadWritePaths=-` con el `workdir` de cada
proyecto declarado (el guion: si un `workdir` desaparece, la unidad igual arranca), `UMask=0002`,
`MemoryMax=512M` y `TasksMax=256`. El techo alto solo se usa durante un deploy: en reposo el agent
sigue midiendo menos de 30 MB. Los `workdir` bajo `/home` no se aceptan (`ProtectHome=true` los
esconde). El resto del endurecimiento no cambia.

`UMask=0002` es para lo que escribe `git`: con el `0077` de la unidad base, un archivo nuevo del
commit quedaría `0600`, `docker build` lo copiaría así a la imagen y una app que no corre como root
no lo podría leer. Los archivos propios del agent (su estado) se crean con permisos explícitos, así
que no cambian.

## Consecuencias

**Positivas**

- Ningún camino del control plane ni del LLM llega a un argumento libre: los comandos salen de una
  tabla fija y los valores, del archivo que escribió una persona o de un SHA validado.
- Con el control plane comprometido, lo peor que se puede pedir es desplegar un commit que ya está
  en la rama protegida, o volver al anterior. Y solo donde una persona prendió el deploy.
- Un deploy que falla deja el sitio como estaba, sin recompilar.

**Negativas**

- El `workdir` tiene que ser escribible por el usuario `statussv`, y el `.env` legible: hay que
  ajustar los permisos de cada proyecto una vez (lo explica el runbook). `statussv` ya era
  equivalente a root por el grupo `docker` (ADR-011), así que no se agrega poder, pero sí archivos
  que tocar.
- Una migración de base no se deshace sola.
- Las imágenes de rollback ocupan disco hasta que alguien corre `docker image prune -a`, y ese
  `prune` puede borrar la imagen a la que se quería volver: el rollback manual lo detecta en
  `preflight` y no arranca.
- Un registro privado de imágenes necesitaría credenciales en `DOCKER_CONFIG`. Hoy los proyectos
  compilan en el servidor y bajan imágenes públicas; queda en la lista de "más adelante".

## Verificación

Probado el 2026-09-30 en el job `laboratorio` del CI (`deploy/laboratorio/deploy.sh`), con systemd
255, Docker 28.0.4, Compose 2.38.2 y git 2.55, corriendo la unidad real con una sonda que hace como
`statussv` lo mismo que el motor, contra un proyecto Compose mínimo clonado por root:

| Comprobación | Resultado |
| --- | --- |
| Sin `20-deploy.conf`: escribir en el `workdir` y `git fetch` | rechazados, como tiene que ser |
| `git` sin `safe.directory` sobre un `workdir` de root | se niega; con `-c safe.directory` funciona |
| `fetch` de un repo, `merge-base --is-ancestor`, `status` limpio y `checkout -B` | funcionan |
| Modo del archivo nuevo después del checkout | `664` con `UMask=0002` |
| Hook `post-checkout` plantado en el `workdir` | no corre (`core.hooksPath=/dev/null`) |
| `ssh` a github.com con la deploy key, `BatchMode` y `known_hosts` propio | negocia; GitHub rechaza la llave, que es lo esperado |
| `docker compose build`, `run --rm`, `up -d --no-build` | funcionan; el contenedor queda `healthy` y el sitio responde |
| Etiquetar la imagen por la API de Docker | `201` |
| Escribir en `/etc` o en el `workdir` de otro proyecto | rechazado |
| Pico de memoria de la unidad con los techos base (64M) | **64 MiB: toca el techo** con un proyecto de juguete |
| Pico de memoria y de tareas con `20-deploy.conf` | 45 MiB y 26 tareas |

Con los techos de la unidad base el deploy de prueba terminó, pero la memoria llegó exactamente al
máximo: con un build real no alcanza. De ahí `MemoryMax=512M`. `TasksMax=256` queda con margen
sobre las 26 medidas.

## Alternativas consideradas

- **Construir la escritura recién después de mudar el control plane:** frena la Fase 2 por una
  decisión de infraestructura. El interruptor en el agent separa las dos cosas.
- **Usar el `origin` y las credenciales que ya tiene cada `workdir`:** son de root, y leerlas le
  daría al agent las credenciales de cada cliente. Descartado.
- **Desplegar imágenes que compila el CI (GHCR):** es lo correcto a largo plazo y está en la deuda
  del Sprint 0, pero hoy ningún proyecto de cliente publica imágenes. Descartado para este sprint.
- **`docker compose up --wait`:** no distingue un contenedor de una sola vez que terminó bien de
  uno caído, y no sabe del healthcheck HTTP. Descartado a favor de la ventana propia.
- **`git reset --hard` en vez de exigir el árbol limpio:** borraría en silencio un arreglo hecho a
  mano en el servidor. Descartado.
- **Correr los hijos en unidades transitorias (`systemd-run`)** para sacarlos del techo de memoria
  del agent: necesita privilegios que el agent no tiene. Descartado.
