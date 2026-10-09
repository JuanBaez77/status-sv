# ADR-022: El alta de un proyecto: el control plane propone y una persona acepta en el servidor

- **Estado:** aceptada
- **Fecha:** 2026-10-08

## Contexto

[ADR-021](ADR-021-primer-deploy-y-reparto-del-deploy-desde-cero.md) dejó para el Sprint 6 tres
decisiones en las que el diseño y los ADR vigentes se contradicen. Este ADR resuelve dos: si el
control plane puede declarar un proyecto (`register_project`) y dónde se cargan los valores de los
secretos. La tercera, las plantillas, está en [ADR-023](ADR-023-plantillas-sitio-y-certificado.md).

Hoy dar de alta un proyecto son siete pasos por SSH (runbook, "El primer deploy de un proyecto
nuevo"): declararlo, la deploy key, el `workdir`, los secretos, el usuario y la unidad, el sitio y
el certificado, y correr el instalador de nuevo. El diseño quiere que eso salga del panel y, en la
Fase 3, del MCP.

Las dos contradicciones tienen el mismo fondo:

- **Declarar un proyecto es poder ejecutar código en el servidor.** Un proyecto trae su `compose`
  o sus pasos declarados, que corren ahí; con Docker, eso equivale a root. Si el control plane
  pudiera declarar, quien lo tome tendría root en cada servidor con el deploy prendido.
  [ADR-012](ADR-012-proyectos-declarados-en-el-servidor.md) lo impide: los proyectos son archivos
  que escribe una persona.
- **Un secreto cargado en el panel pasa por el control plane**, y la regla 3 y
  [ADR-011](ADR-011-acceso-a-docker-y-journald.md) dicen que ni un control plane comprometido los
  ve.

Al revisar el código apareció una tercera restricción: el agent solo puede escribir en los
`workdir` que el instalador encontró al prender el deploy, un `ReadWritePaths` por proyecto en el
drop-in `20-deploy.conf`. Un proyecto nuevo obliga a reescribir ese archivo y a reiniciar el agent,
y las dos cosas necesitan root.

La decisión la tomó Juan el 2026-10-08, aceptando las recomendaciones del plan.

## Decisión

**1. El control plane propone; no declara.**

Una propuesta es una declaración completa, con el mismo formato y el mismo parser estricto que un
`project.yaml` (el parser pasa a `internal/projectspec`, compartido por el agent, el ayudante y el
control plane). La arma una persona desde el panel y, en el Sprint 8, el MCP con
`register_project`. No cambia nada en ningún servidor hasta que una persona la acepta en él.

En una propuesta, lo que toca el sistema de archivos o los usuarios del servidor no se elige:

| Campo | En una propuesta | Por qué |
| --- | --- | --- |
| `workdir` | siempre `/srv/<nombre>` | el comando de aceptación lo crea como root; elegido libre, podría apuntar a cualquier directorio |
| `run_as` (systemd) | siempre `<nombre>`, que entonces empieza con letra y tiene hasta 32 caracteres (lo que acepta `useradd`) | elegido libre, el código del proyecto podría correr como `postgres` o `www-data` |
| `env_file` (systemd) | siempre `/etc/status-sv/env/<nombre>.env` | systemd lo lee como root y se lo pasa al proyecto: elegido libre, podría ser `/etc/shadow` |
| `units` (con `service`) | siempre `[<nombre>.service]` | es la unidad que se genera ([ADR-023](ADR-023-plantillas-sitio-y-certificado.md)) |

Lo demás (repo, rama, runtime, Compose, healthcheck, pasos declarados, dominio, puerto y las
variables requeridas) es lo que la persona revisa al aceptar.

El repo de una propuesta no lleva credenciales (ni `usuario:contraseña@`, ni un token en una
dirección `https`): quedarían en la base del control plane. Para un repo privado está la deploy
key. Lo agregamos el 2026-10-09, al escribir los tests del lado del servidor.

- Hay una propuesta pendiente por nombre y por servidor: una nueva con el mismo nombre reemplaza a
  la anterior. No se puede proponer el nombre de un proyecto ya declarado en ese servidor.
- Vence a los 7 días sin aceptar. Se puede retirar.
- Proponer no pide código TOTP: no cambia nada. Queda en `audit_log` (`project.propose`,
  `project.withdraw`), con el actor y su tipo (persona o LLM).
- `register_project` por MCP no pide aprobación: la aprobación es la aceptación en el servidor.

**2. La propuesta viaja al agent como un mensaje, no como un comando.**

`project_proposal` (servidor → agent) trae el ID de la propuesta, el nombre, el texto YAML y el
vencimiento, o el pedido de retirarla. No es una operación de la lista cerrada de la regla 2: el
agent no ejecuta nada con ella.

- El agent la valida con el parser estricto y la guarda en
  `/var/lib/status-sv/proposals/<nombre>.yaml`, en su directorio de estado. Como máximo 10
  pendientes; las vencidas las borra.
- La acusa con `project_proposal_ack`, que dice si la guardó o por qué no.
- `projects_state` suma las propuestas pendientes (ID, nombre y hash), así el control plane sabe
  qué llegó y reenvía lo que falte al reconectar.

**3. Una persona la acepta en el servidor con un comando.**

```bash
sudo statussv-agent project accept <nombre>
```

| Paso | Qué hace |
| --- | --- |
| Revisar | Que corra como root, que el deploy esté prendido en el servidor, que la propuesta exista, sea válida y no esté vencida, que el nombre no esté declarado con otro contenido, y que **no haya un deploy en curso** (reiniciar el agent en el medio lo haría volver atrás) |
| Mostrar | La declaración entera y lo que va a crear: usuario, `workdir`, llave, archivo de secretos, la unidad y el sitio tal como van a quedar. Pide escribir el nombre del proyecto para seguir |
| Usuario | Con systemd, `useradd --system` del usuario del proyecto, sin shell ni home |
| `workdir` | `/srv/<nombre>` con el dueño y los permisos del runbook |
| Llave | Una deploy key ed25519 en `/etc/status-sv/keys/<nombre>`, salvo `--no-deploy-key` (repo público). Muestra la llave pública y el comando `gh repo deploy-key add` para cargarla en GitHub |
| Secretos | Pide el valor de cada variable de `required_env` sin mostrarlo (punto 4) |
| Unidad y sitio | Desde las plantillas ([ADR-023](ADR-023-plantillas-sitio-y-certificado.md)) |
| Declarar | Escribe el `project.yaml` en `/etc/status-sv/projects.d`, `root:statussv 0640`: **exactamente el texto que mostró** |
| Prender | Reescribe `20-deploy.conf` con el `workdir` nuevo y reinicia el agent |
| Revisar el deploy | `deploy-check` del proyecto, como lo corre el instalador |

- **La propuesta es texto no confiable**: la escribió el agent, que puede estar comprometido, y la
  armó el control plane, que también. El comando la vuelve a validar, tiene un tope de tamaño y
  escribe solo lo que mostró.
- Si un paso ya está hecho (el usuario o el `workdir` existen de un intento anterior), lo dice y
  pregunta antes de usarlo. Correrlo de nuevo continúa donde quedó.
- `--yes` y `--env-file` existen para la prueba de punta a punta del CI.
- La generación de `20-deploy.conf` pasa del instalador al binario (`statussv-agent deploy-dropin`),
  para que el instalador y este comando escriban lo mismo.
- Sin el deploy prendido en el servidor, el comando se niega: prenderlo es una decisión por
  servidor ([ADR-015](ADR-015-motor-de-deploy-docker.md)) y no se toma de costado.

**4. Los valores de los secretos se cargan en el servidor y no salen de él.**

- El proyecto declara **solo los nombres** en `required_env` (hasta 20, `^[A-Z_][A-Z0-9_]*$`).
  Al construirlo, el tope bajó de 50 a 20: los nombres que faltan viajan en `projects_state`,
  por cada proyecto del servidor, y con 50 el mensaje podía pasar de 256 KiB.
- Los valores se piden en `project accept`, y se cambian después con
  `sudo statussv-agent project env <nombre>`.
- Con Docker van al `.env` del `workdir`, `root:statussv 0640`, que es el que lee
  `docker compose`. Con systemd van a `/etc/status-sv/env/<nombre>.env`, `root:root 0600`, que
  lee systemd para el servicio y para los pasos.
- Lo único que viaja al control plane son **los nombres que faltan**: con Docker los mira el
  agent en el `.env`; con systemd, el ayudante en su operación `check`, que ahora los devuelve.
- El primer deploy se frena en el chequeo previo si falta alguna variable, sin tocar nada.

**5. La aceptación vuelve al control plane por lo que el agent ya informa.**

`projects_state` suma el hash de cada `project.yaml` declarado. Cuando aparece declarado un
proyecto con el nombre de una propuesta, la propuesta queda aceptada y `audit_log` registra
`project.accept` con el servidor como actor, y si lo declarado es igual a lo propuesto o lo
cambió alguien.

**6. El primer deploy no cambia, salvo dos cosas.**

Sigue pidiendo siempre el código TOTP ([ADR-021](ADR-021-primer-deploy-y-reparto-del-deploy-desde-cero.md)).
Se frena si faltan variables (punto 4) y, cuando termina bien con systemd, habilita las unidades
con una operación nueva del ayudante, `enable_units`, que como las demás recibe solo el nombre del
proyecto. Hasta ahora eso era un `systemctl enable` a mano.

**7. La deploy key no se cifra.**

El diseño decía "se guarda cifrada en el servidor". El agent la necesita en claro para traer el
código, y cifrarla con una clave que está en el mismo servidor no protege nada. Sigue en
`/etc/status-sv/keys/<nombre>`, `root:statussv 0640`. La llave pública viaja en `projects_state`
para que el panel la muestre (es pública). Revocarla es sacarla de GitHub y borrar el archivo; desde
el panel queda para más adelante.

## Consecuencias

**Positivas**

- [ADR-012](ADR-012-proyectos-declarados-en-el-servidor.md) sigue tal cual: un control plane
  comprometido solo puede proponer, y nada corre hasta que una persona con root lee la propuesta.
- Los siete pasos por SSH pasan a ser un comando y cargar la llave en GitHub.
- Los valores de los secretos no pasan nunca por el control plane, ni por el panel ni por el LLM.
- `register_project` por MCP no suma riesgo: propone, igual que el panel.

**Negativas**

- Cada proyecto nuevo sigue necesitando una persona en el servidor, por SSH.
- `project accept` es un programa de status-sv que corre como root y lee un archivo que escribió
  el agent. Es más grande que el ayudante; lo mitiga que lo corre una persona, que ve lo que va a
  escribir.
- Aceptar reinicia el agent: unos segundos sin métricas de ese servidor.
- El `workdir`, el usuario y el archivo de secretos de una propuesta no se eligen.

## Alternativas consideradas

- **Que el control plane declare directo**, con el ayudante escribiendo el `project.yaml`: sin
  SSH, pero un control plane comprometido tendría root en cada servidor con deploy. Descartado.
- **Secretos en el panel, cifrados en el navegador** con una clave del servidor: el código del
  panel lo sirve el mismo control plane, que podría cambiar la clave o leer lo que se escribe.
  Descartado.
- **Secretos en el panel, en claro:** rompe la regla 3. Descartado.
- **Sin mensaje nuevo:** el panel muestra un comando con la declaración adentro, para pegar en el
  servidor. Sin cambios en el agent, pero es un pegado largo, no hay forma de saber si se aceptó y
  no le sirve al MCP. Descartado.
- **`ReadWritePaths=/srv` en el drop-in**, para no reiniciar el agent: le da escritura en todo
  `/srv`. Descartado.
