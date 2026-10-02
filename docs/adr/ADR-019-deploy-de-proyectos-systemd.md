# ADR-019: Deploy de proyectos systemd, con un ayudante root de operaciones fijas

- **Estado:** aceptada, salvo el punto 3. El plan que aprobó Juan decía "una regla de polkit"; acá
  es un ayudante root, por lo que cuenta el Contexto. Ese punto espera su visto bueno y el
  laboratorio del Sprint 5.
- **Fecha:** 2026-10-02

## Contexto

El Sprint 5 cierra la Fase 2: además de los proyectos Docker
([ADR-015](ADR-015-motor-de-deploy-docker.md)), hay que desplegar los que corren como servicio de
systemd (gunicorn, por ejemplo), con verificación y rollback automático.

Con Docker, el código del proyecto corre adentro de un contenedor, que arranca el demonio de
Docker. Con systemd no hay contenedor, y eso cambia tres cosas:

- **Instalar dependencias y migrar es ejecutar código del repositorio en el servidor.** Un
  `pip install` corre el código de instalación de cada paquete, y `manage.py migrate` corre el del
  proyecto. Si eso fuera un proceso hijo del agent, correría como `statussv`: con la credencial
  del agent en `/var/lib/status-sv`, las deploy keys de todos los proyectos del servidor y el
  socket de Docker al alcance. Y adentro del sandbox del agent, que tiene
  `MemoryDenyWriteExecute`, 512 MB y el filtro de syscalls.
- **El agent no puede reiniciar un servicio.** Su unidad no tiene capacidades, lleva
  `NoNewPrivileges` (así que `sudo` no existe para él) y systemd le niega `RestartUnit`.
- **No hay imagen a la que volver.** El rollback de Docker reetiqueta la imagen anterior; acá lo
  anterior es un commit y un entorno virtual.

Las decisiones 2, 3 y 4 del plan las tomó Juan el 2026-10-02, aceptando las recomendaciones. La
recomendación 2 decía: unidades oneshot escritas por root, que el agent arranca por D-Bus con una
regla de polkit limitada a esos nombres. Antes de escribir este ADR se midió en el laboratorio:

| Comprobación (systemd 255, polkit 124) | Resultado |
| --- | --- |
| Usuario sin privilegios arranca una unidad, sin regla | rechazado |
| Con una regla de polkit que nombra la unidad y el verbo | funciona |
| Con esa regla, otra unidad | rechazado |
| Con esa regla, una unidad transitoria | rechazado |
| Proceso root sin capacidades arranca una unidad transitoria con otro usuario | funciona |
| Proceso root sin capacidades reinicia una unidad | funciona |

La regla por unidad funciona, pero necesita polkit 0.106 o más nuevo, que es el que entiende
reglas en JavaScript. **Ubuntu 22.04 trae polkit 0.105** (`policykit-1 0.105-33ubuntu0.2`), que
solo sabe dar el permiso `manage-units` entero: cualquier unidad, incluidas las transitorias, que
es root. `sv-gcg`, el servidor donde hoy está prendido el deploy, es Ubuntu 22.04.

## Decisión

**1. El mismo motor y los mismos pasos; cambia lo que hace cada uno.**

| Paso | Docker (ADR-015) | systemd |
| --- | --- | --- |
| `preflight` | `workdir`, repo, árbol limpio, Compose | `workdir`, repo, árbol limpio, que las unidades declaradas existan y que el ayudante responda |
| `fetch` | igual | igual |
| `snapshot` | commit e imágenes | commit |
| `checkout` | igual | igual |
| `build` | `docker compose build` | los pasos de `deploy.build`, en orden |
| `pre` | pasos en un contenedor | los pasos de `deploy.pre` |
| `up` / `restart` | `docker compose up -d` | reiniciar las unidades de `units`, en el orden declarado |
| `healthcheck` | contenedores y HTTP | unidades y HTTP (punto 6) |
| `post` | pasos en un contenedor | los pasos de `deploy.post` |

`git` lo sigue ejecutando el agent, con las mismas reglas de ADR-015: es su código de siempre,
sobre un repositorio, y no ejecuta nada del proyecto (los hooks están apagados).

**2. El código del proyecto no corre nunca como hijo del agent.**

Cada paso declarado corre en una unidad transitoria de systemd, de una sola vez, con el usuario
del proyecto (`run_as`), parada en el `workdir`, con su archivo de entorno si declara uno y con el
tiempo máximo del paso. Queda afuera del sandbox y del cgroup del agent: no ve su credencial ni
las llaves, y no cuenta contra su memoria.

- La salida del paso va al diario. El agent la lee con `journalctl`, como ya lee los logs
  ([ADR-011](ADR-011-acceso-a-docker-y-journald.md)), por el nombre de la unidad, que es único por
  deploy y por paso (`statussv-step-<deploy>-<fase>-<n>.service`). Pasa por el mismo filtro que
  enmascara secretos.
- La unidad lleva `NoNewPrivileges`, `/tmp` propio y el sistema en solo lectura fuera del
  `workdir` y del directorio personal de `run_as`. El archivo de entorno lo lee systemd; el agent
  no lo abre.

**3. Lo que necesita root lo hace un ayudante mínimo, con tres operaciones fijas.**

El ayudante es el mismo binario (`statussv-agent unit-helper`), corrido como root por una unidad
propia que se activa por un socket unix. Lo instala `install-agent.sh --enable-deploy`; sin eso no
existe en el servidor.

| Operación | Parámetros | Qué hace |
| --- | --- | --- |
| `run_step` | proyecto, fase (`build`, `pre`, `post`), número de paso, ID del deploy | Arranca la unidad transitoria de ese paso y espera a que termine |
| `stop_step` | los mismos | Para esa unidad (tiempo agotado o deploy cortado) |
| `restart_units` | proyecto | Reinicia las unidades declaradas del proyecto |

- **El agent no nombra unidades ni comandos: nombra un proyecto y un número de paso.** El ayudante
  lee él mismo el `project.yaml` de `/etc/status-sv/projects.d`, con el mismo parser estricto, y de
  ahí saca el comando, el usuario y las unidades. Un agent comprometido puede pedir que corra un
  paso declarado o que se reinicie un servicio declarado, y nada más.
- El socket es `root:statussv 0660`, y el ayudante verifica además que quien se conecta sea el
  usuario del agent (credenciales del socket).
- Si la conexión se corta mientras corre un paso, el ayudante para la unidad: un paso no queda
  corriendo sin un deploy que lo espere.
- El ayudante corre sin capacidades, sin red, con el sistema de archivos en solo lectura y sin
  estado propio. Habla con systemd por el bus, que a root lo deja pasar.
- Como lee la declaración en cada pedido, no hay archivos generados que puedan quedar viejos:
  cambiar el `project.yaml` no obliga a correr el instalador de nuevo.

**4. El `project.yaml` de un proyecto systemd.**

```yaml
name: ypf-aca
environment: production
repo: github.com/extendsystem/ypf-aca
branch: main
runtime: systemd
workdir: /srv/ypf-aca
units: [gunicorn-ypf-aca.service]
run_as: ypfaca
env_file: /etc/ypf-aca/env        # opcional
healthcheck:
  url: http://127.0.0.1:8001/health
deploy:
  build:
    - command: [".venv/bin/pip", "install", "-r", "requirements.txt"]
      timeout_seconds: 600
    - command: [".venv/bin/python", "manage.py", "collectstatic", "--noinput"]
  pre:
    - command: [".venv/bin/python", "manage.py", "migrate", "--noinput"]
  post: []
rollback: previous_commit
```

- `run_as` es obligatorio con systemd: un usuario del sistema que no sea `root` ni el del agent.
- `env_file` es opcional: una ruta absoluta. De ese archivo viaja al control plane que existe,
  nunca su contenido.
- Un paso es un `command` como lista de argumentos y un `timeout_seconds`, sin `service`. El
  primer argumento es una ruta: absoluta, o relativa al `workdir` y sin salir de él. No se busca
  en el `PATH`: lo que corre es exactamente lo que se escribió.
- `build` es nuevo y solo de systemd: lo que prepara el código (dependencias, estáticos). `pre` y
  `post` son los de [ADR-016](ADR-016-pasos-declarados-del-deploy.md), con las mismas reglas: como
  máximo 5 pasos por lista, del archivo y no del pedido.
- No es un formato de Python: sirve para cualquier servicio que se actualice con comandos y un
  reinicio.

**5. El rollback vuelve al commit anterior, corre `build` de nuevo y reinicia.**

- No corre `pre`: una migración no se deshace, como en ADR-016.
- Vale para el rollback automático y para el manual. El automático verifica solo las unidades,
  con la ventana corta; el manual, con la ventana completa.
- Depende de que `build` deje el entorno como estaba: con versiones fijadas en
  `requirements.txt`, `pip` vuelve a las anteriores, desde su caché si las tiene.
- Se guardan los últimos 3 commits buenos, como con Docker.

**6. La verificación mira las unidades y el healthcheck HTTP.**

Cada unidad de `units` tiene que estar `active` y `running`, con el mismo proceso principal y sin
reinicios nuevos entre una mirada y la siguiente: un gunicorn que arranca, muere y systemd
levanta de nuevo no cuenta como sano. Lo demás es ADR-015: cada 2 s, tres veces seguidas bien,
hasta 120 s, junto con el healthcheck HTTP declarado.

**7. El contrato suma poco.**

- El paso `restart`, y `build` puede aparecer más de una vez (uno por paso declarado).
- El código `restart_failed`.
- El mismo interruptor: sin `STATUSSV_DEPLOY_ENABLED` el agent rechaza con `disabled`, y sin el
  ayudante instalado el chequeo previo falla y lo explica.

## Consecuencias

**Positivas**

- Un mecanismo solo, igual en Ubuntu 22.04 y 24.04, sin depender de la versión de polkit.
- El código de un repositorio corre con el usuario de su proyecto, no con el del agent: un
  paquete malicioso no llega a la credencial del agent, a las llaves de otros proyectos ni al
  socket de Docker.
- Ninguna operación acepta texto: ni el control plane ni el agent pueden hacer correr un comando
  que no esté escrito en el archivo de root.
- Los pasos corren con el entorno real del servicio (`env_file`), que el agent no necesita leer.

**Negativas**

- **Hay un proceso root nuevo de status-sv en cada servidor con el deploy prendido.** Es chico,
  se activa a demanda y no tiene red ni capacidades, pero es root: un error en él es más grave
  que uno en el agent. Por eso sus tres operaciones no reciben más que identificadores.
- El `workdir` necesita dos permisos: escritura del grupo `statussv` para `git` (como con
  Docker) y que `run_as` pueda leer el código y escribir donde escriben sus pasos (el entorno
  virtual, los estáticos). Lo explica el runbook.
- **El rollback no es exacto si las dependencias no tienen versión fijada**, y necesita la red o
  la caché de `pip` si cambiaron. Una copia del entorno virtual lo resolvería; queda en "más
  adelante".
- Un servicio que hoy corre como root no se puede declarar hasta darle un usuario propio.
- Reiniciar no es recargar: un `restart` de gunicorn corta los pedidos en curso. El `reload`
  queda en "más adelante".

## Alternativas consideradas

- **Regla de polkit por unidad**, con unidades fijas que escribe el instalador. Es lo que decía
  el plan y es menos código propio, pero no existe en Ubuntu 22.04, obliga a generar archivos de
  unidad (con el escapado de sus argumentos) y a correr el instalador cada vez que cambia un
  `project.yaml`. Con dos mecanismos según la versión, habría que probar y mantener los dos.
- **Dar `manage-units` entero al agent** (lo único que permite polkit 0.105): es root, incluso en
  un servidor sin Docker. Descartado.
- **Correr los pasos como hijos del agent**, como `git`: lo más simple, pero el código del
  repositorio correría con el usuario del agent y adentro de su sandbox. Descartado.
- **Unidades `.path`** que disparan un servicio cuando el agent toca un archivo: sin proceso
  propio, pero sin respuesta, sin parámetros y con los límites de disparo de systemd en el medio.
  Descartado.
- **`sudo` con comandos fijos:** `NoNewPrivileges` lo impide, y sacarlo abriría bastante más que
  esto.
- **Un directorio por release con un enlace simbólico**, al estilo Capistrano: rollback exacto,
  pero obliga a rearmar cómo está instalado cada proyecto que ya corre. Descartado para este
  sprint.
