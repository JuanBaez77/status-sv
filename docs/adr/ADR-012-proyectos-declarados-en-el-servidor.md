# ADR-012: Los proyectos se declaran en archivos del propio servidor

- **Estado:** aceptada
- **Fecha:** 2026-09-28

## Contexto

El principio 3 del diseño dice que solo se despliega lo declarado en un `project.yaml`, y la regla 2
del proyecto limita el agent a operar sobre proyectos declarados. El Sprint 3 empieza a leer esas
declaraciones (todavía sin desplegar nada), así que hay que fijar dónde viven, quién las escribe,
cómo se validan y cómo se liga un proyecto con los contenedores y servicios que el agent ya ve.

El diseño no dice dónde vive el archivo. Había tres lugares posibles:

- **En el repo de cada proyecto.** Un PR podría cambiar su propia declaración, incluidos los pasos
  `pre` y `post`. El texto de un PR no es confiable (regla 9): un LLM engañado podría proponer un
  paso que después corre en producción.
- **En la base del control plane**, editado desde el panel o por MCP. Si alguien toma el control
  plane, puede declarar lo que quiera, y la tabla de amenazas del diseño promete lo contrario: con
  el control plane comprometido, el agent solo opera sobre lo declarado.
- **En el servidor donde corre el agent**, escrito por una persona.

## Decisión

**1. Cada proyecto es un archivo en `/etc/status-sv/projects.d/<nombre>.yaml`**, por decisión de Juan.

- El directorio es `root:statussv 0750` y los archivos `root:statussv 0640`. El agent los lee y no
  puede escribirlos: su unidad ya tiene `ProtectSystem=strict`, que deja `/etc` en solo lectura.
- Los escribe una persona con acceso SSH. El control plane no tiene ningún camino para crear ni
  cambiar un proyecto; solo recibe lo que el agent leyó. El alta desde el panel o por MCP
  (`register_project`, Sprint 5 y 7) se decide cuando se construya, sobre esta base.

**2. El formato es el del diseño, con tres ajustes.**

- **`units`** (nuevo): para `runtime: systemd`, la lista de servicios del proyecto
  (`gunicorn-cristales.service`). Es obligatoria con systemd y no se acepta con Docker.
- **`compose_project`** (nuevo, opcional): el nombre del proyecto de Compose. Si falta, es el
  nombre del directorio de `workdir` normalizado como lo hace Compose, que es lo que Compose usa
  cuando nadie lo fija.
- **`server` se quita.** El archivo vive en el servidor, así que el campo sobra y podría contradecir
  al servidor real.

**3. El parser es estricto.** Un archivo con cualquier error queda afuera entero; no se aplica a medias.

- Los campos desconocidos son error. Tope de 64 KiB por archivo y de 50 proyectos por servidor.
- `name` coincide con el nombre del archivo y cumple `^[a-z0-9][a-z0-9-]{0,62}$`.
- `environment` es `production` o `staging`. `runtime` es `docker` o `systemd`.
- `workdir` es absoluta, limpia (sin `..`) y no es `/`. `compose_file` es relativa y queda adentro
  de `workdir`.
- `healthcheck.url` es `http` o `https`, sin usuario ni contraseña. `expect_status` va de 100 a 599
  y `timeout_seconds` de 1 a 30.
- `units` son nombres de unidad válidos que terminan en `.service`, como máximo 10.
- **`deploy.pre` y `deploy.post` tienen que estar vacíos** hasta que el Sprint 4 defina el formato
  de los pasos declarados. Así ningún archivo escrito hoy trae pasos que después se ejecuten con
  reglas que todavía no existen.
- `rollback` solo acepta `previous_image`, y solo con Docker. El de systemd lo define el Sprint 5.

**4. El agent manda lo que leyó en un mensaje propio, `projects_state`.**

- Relee el directorio al arrancar y cada 30 s mira si cambiaron las fechas de modificación.
  Manda solo si algo cambió: una foto en vuelo, con acuse y reenvío, igual que `runtime_state`.
- De un archivo inválido viaja el nombre del archivo y el error (qué campo y por qué), **nunca su
  contenido**.
- No viaja nada secreto: el formato no tiene campos para secretos. Las variables requeridas del
  bootstrap (Sprint 5) van a llevar solo nombres.

**5. El ligado con las unidades lo calcula el control plane al leer.**

- Docker: el contenedor cuya etiqueta `com.docker.compose.project` coincide con `compose_project`.
  El agent ya la manda desde el Sprint 2.
- systemd: los servicios que aparecen en `units`.
- No se guarda el ligado: se cruza `projects` con `runtime_units` en cada consulta. Son decenas
  de filas.

**6. El agent revisa el healthcheck declarado cada 60 s** (healthcheck pasivo).

- Hace un `GET` a la URL declarada, con el timeout del archivo, verificando el certificado y **sin
  seguir redirecciones**: una respuesta 3xx vale como su código, y si se espera 301 se declara 301.
- De la respuesta solo lee el código y el tiempo; descarta el cuerpo.
- El resultado viaja en `projects_state`, pero solo cuando el estado cambia (de bien a mal o al
  revés). Mientras siga igual, la hora del último chequeo viaja cada 5 minutos, igual que el
  consumo en `runtime_state`.
- Es la línea de corte del sprint: si no entra, se mueve al Sprint 4 sin tocar lo demás.

## Consecuencias

**Positivas**

- Con el control plane comprometido no se puede declarar un proyecto nuevo ni agregarle pasos. Para
  cambiar una declaración hay que entrar al servidor.
- Una declaración no cambia por un PR ni por texto que lea un LLM.
- El agent no gana permisos: `/etc` ya era legible y sigue siendo de solo lectura.

**Negativas**

- Declarar un proyecto requiere SSH. Con pocos proyectos por servidor es aceptable, y el diseño ya
  prevé un alta asistida en el Sprint 5.
- Si alguien cambia un archivo, el panel tarda hasta 30 s en verlo.
- El healthcheck sale desde el mismo servidor, así que no detecta un problema de DNS o de red que
  solo se ve desde afuera. Eso lo cubre el smoke test de la Fase 4.

## Alternativas consideradas

- **`project.yaml` en el repo del proyecto:** descartado por el riesgo de que un PR cambie su propia
  declaración.
- **Declaración en la base del control plane:** descartada; rompe la garantía ante un control plane
  comprometido.
- **Mirar el directorio con inotify en vez de revisar cada 30 s:** suma una dependencia y otro
  permiso que evaluar, para ganar segundos en algo que cambia muy de vez en cuando.
- **Seguir las redirecciones del healthcheck:** permitiría que una redirección lleve al agent a
  cualquier host. Descartado.
