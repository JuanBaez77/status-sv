# ADR-021: El deploy desde cero se parte: primer deploy ahora, lo que necesita root después

- **Estado:** aceptada
- **Fecha:** 2026-10-02

## Contexto

El diseño describe el deploy desde cero en seis pasos: clonar, cargar secretos, compilar y
arrancar, armar la entrada web (bloque del proxy y certificado), correr los pasos declarados y
verificar. Lo ubica al cierre de la Fase 2, en el Sprint 5, y avisa que si no entra se mueve al
Sprint 6.

Al planificarlo aparecieron cuatro cosas:

- **Tres de los seis pasos necesitan root:** crear el directorio de trabajo, escribir la unidad
  de systemd y tocar el proxy y el certificado. El agent no tiene capacidades ni las puede ganar.
- **El diseño dice que los valores de los secretos se cargan en el panel.** Eso los hace pasar
  por el control plane, y la regla 3 y [ADR-011](ADR-011-acceso-a-docker-y-journald.md) dicen que
  ni un control plane comprometido ve secretos de los proyectos.
- **`register_project` declara un proyecto desde el control plane**, y
  [ADR-012](ADR-012-proyectos-declarados-en-el-servidor.md) dice que el control plane no puede
  crear ni cambiar un proyecto.
- **El tamaño.** El deploy Docker solo llevó trece PR y tres arreglos.

La decisión la tomó Juan el 2026-10-02.

## Decisión

**1. En el Sprint 5 entra el primer deploy: clonar y levantar un proyecto ya declarado.**

Es la operación `bootstrap` de la lista del agent, sobre un proyecto que una persona declaró y
preparó en el servidor:

| Paso | Qué hace | Si falla |
| --- | --- | --- |
| `preflight` | El proyecto no tiene ningún estado bueno anotado, nada suyo corre, el `workdir` existe y todavía no tiene el código (o tiene el clon de un intento anterior), y el servidor tiene con qué correrlo | Nada que deshacer |
| `fetch` | Inicia el repositorio en el `workdir` y trae la rama declarada, con la llave y los host keys del proyecto | Nada que deshacer |
| `checkout` | Deja el código en la punta de la rama y, ya con el código, revisa lo que antes no se podía (el archivo de Compose) | Nada que deshacer |
| `build`, `pre` | Como en un deploy | Nada corre todavía |
| `up` / `restart` | Levanta los contenedores o arranca las unidades | Baja lo que levantó |
| `healthcheck`, `post` | Como en un deploy | Baja lo que levantó |

- No hay `snapshot` ni vuelta a un estado anterior: no existe. Deshacer es bajar lo que este
  primer deploy levantó (`docker compose down`, sin borrar volúmenes, o parar las unidades), y el
  deploy termina `rolled_back`. Si falló antes de levantar nada, termina `failed`.
- **El código clonado no se borra si falla.** El agent no borra directorios, y el clon sirve para
  mirar qué pasó. El intento siguiente lo acepta mientras el proyecto no tenga un estado bueno.
- Con un estado bueno anotado, o con algo del proyecto corriendo (contenedores suyos, o una de sus
  unidades activa), el primer deploy termina con el código `already_deployed` sin tocar nada: lo
  que sigue son deploys.
- El primer deploy es siempre de la punta de la rama declarada: no lleva commit.

Al construirlo cambiaron dos cosas de lo que se había escrito acá:

- **El `workdir` no tiene que estar vacío: tiene que estar sin clonar.** Un proyecto Docker lee su
  `.env` del `workdir`, y una persona lo deja ahí antes del primer deploy. Lo que se cuida es no
  pisar nada: git no escribe sobre un archivo que ya estaba, así que si algo del `workdir` choca
  con un archivo versionado, el `checkout` falla y no se pierde nada.
- **Parar las unidades de un proyecto systemd necesita root**, así que el ayudante de
  [ADR-019](ADR-019-deploy-de-proyectos-systemd.md) suma una operación fija, `stop_units`, que
  recibe solo el nombre del proyecto.

`deploy-check` reconoce un proyecto que todavía no tiene el código: en vez de darlo por roto, prueba
que el agent pueda escribir en el `workdir` y que el repo conteste con la llave del proyecto, sin
traer nada.

**2. Siempre con la confirmación completa: el nombre del proyecto y un código TOTP.**

También en `staging`. La regla 4 dice que el bootstrap siempre exige aprobación; mientras el
disparo sea desde el panel, la aprobación es la de la persona que lo pide con el segundo factor
en la mano ([ADR-017](ADR-017-deploy-desde-el-panel-y-auditoria.md)). Por MCP va a devolver
`pending_approval`, como toda escritura.

**3. Es un tipo más de deploy.**

`kind` suma `bootstrap` en el contrato y en la tabla `deploys` (migración 00010), y comparte el
historial, el avance en vivo, el bloqueo de simultáneos y `audit_log` (`bootstrap.request`).

**4. Lo que necesita root lo sigue haciendo una persona por SSH, con una lista en el runbook.**

Declarar el proyecto, generar la deploy key, crear el `workdir` con sus permisos, escribir el
archivo de entorno con los secretos, y, si hace falta, la unidad del servicio, el bloque del
proxy y el certificado. Después, el instalador y `deploy-check`.

**5. Pasa al Sprint 6, con su propio ADR antes de construir:**

- Generar la unidad de systemd y el bloque del proxy desde plantillas con parámetros validados,
  y pedir el certificado. Lo haría el ayudante root de
  [ADR-019](ADR-019-deploy-de-proyectos-systemd.md), que es la única pieza con root.
- Las plantillas editables desde el panel, con vista previa y auditoría.
- Las variables requeridas: qué viaja (nombres) y dónde se cargan los valores.
- `register_project`.

Tres decisiones quedan abiertas para ese ADR, porque el diseño y los ADR vigentes se contradicen:
dónde se cargan los valores de los secretos (el diseño dice el panel; la regla 3 y ADR-011 dicen
que no salen del servidor), si el control plane puede declarar un proyecto (ADR-012 dice que no),
y si las plantillas viven en el control plane o en cada servidor.

**6. El roadmap se corre un sprint.**

La Fase 2 pasa a ser los Sprints 4 a 6 y la Fase 3 (MCP) los Sprints 7 y 8. El criterio de salida
de la Fase 2 no cambia y se cumple en el Sprint 5: un proyecto Docker y uno gunicorn se
despliegan y hacen rollback solos.

## Consecuencias

**Positivas**

- Levantar un proyecto nuevo deja de ser un clon y un `up` a mano, con el mismo rastro que un
  deploy.
- No se construye nada con root, ni nada que mueva secretos, antes de decidir cómo.
- Las contradicciones del diseño quedan escritas en vez de resolverse en el código.

**Negativas**

- El alta de un proyecto sigue necesitando SSH: es un primer deploy, no un deploy desde cero.
- El MCP llega un sprint más tarde.
- Un primer deploy que falla deja el código clonado en el servidor.

## Alternativas consideradas

- **El bootstrap completo en el Sprint 5:** unos diez PR más, con root, secretos y plantillas
  decididos a las apuradas. Descartado.
- **Todo el bootstrap al Sprint 6:** más simple, pero el primer deploy sale casi gratis del motor
  que ya existe y es la parte que se usa cada vez que se suma un proyecto. Descartado; queda como
  línea de corte del sprint.
- **Borrar el clon cuando el primer deploy falla:** deja el `workdir` limpio, pero es el único
  lugar del agent que borraría un directorio entero. Descartado.
