# Contratos agent ↔ control plane

Este paquete es la **única fuente de verdad** de los mensajes que viajan por el WebSocket.
Lo importan los dos extremos, así que el contrato se escribe una sola vez.

El formato es JSON con un envelope versionado; el porqué está en
[ADR-003](../docs/adr/ADR-003-contratos-json-versionado.md).

## Envelope

```json
{
  "v": 1,
  "type": "heartbeat",
  "id": "01924f9c-0000-7000-8000-000000000001",
  "ts": "2026-09-20T12:00:00Z",
  "data": { }
}
```

| Campo | Para qué |
| --- | --- |
| `v` | Versión del protocolo. El receptor rechaza con error explícito lo que no conoce. |
| `type` | Tipo de mensaje, de una lista cerrada. Un tipo desconocido se rechaza, no se ignora. |
| `id` | UUID v7 del mensaje. Sirve para el acuse y para que los comandos sean idempotentes. |
| `ts` | Momento en que se armó el mensaje, en UTC. |
| `data` | El mensaje en sí, según `type`. |

## Mensajes

| Tipo | Dirección | Cuándo | Contenido |
| --- | --- | --- | --- |
| `enroll` | Agent → servidor | Una vez, al instalarse | Token de un solo uso, hostname, versión, sistema y arquitectura |
| `enroll_ack` | Servidor → agent | Respuesta al anterior | ID y nombre del servidor, **credencial propia** y configuración inicial |
| `heartbeat` | Agent → servidor | Cada 15 s | Versión, tiempo encendido y consumo del propio agent |
| `heartbeat_ack` | Servidor → agent | Respuesta al anterior | Hora del servidor y, solo si cambió, la configuración nueva |
| `metrics` | Agent → servidor | Cada 10 s, en lotes | Muestras de CPU, memoria, carga, discos e interfaces |
| `metrics_ack` | Servidor → agent | Respuesta al anterior | Cuántas muestras se guardaron y la marca de tiempo de la última |
| `runtime_state` | Agent → servidor | Al cambiar algo, y cada tanto con el consumo al día | Foto completa de contenedores, servicios y procesos no gestionados |
| `runtime_state_ack` | Servidor → agent | Respuesta al anterior | La marca de tiempo de la foto que se guardó |
| `projects_state` | Agent → servidor | Al conectar y cuando cambia algo | Proyectos declarados en `/etc/status-sv/projects.d` (ADR-012), los archivos inválidos (nombre y error, nunca el contenido) y el último healthcheck |
| `projects_state_ack` | Servidor → agent | Respuesta al anterior | La marca de tiempo de los proyectos que se guardaron |
| `command` | Servidor → agent | A demanda | Una operación de la lista cerrada, con sus parámetros tipados |
| `command_ack` | Agent → servidor | Respuesta al anterior | Cómo terminó el comando y su resultado |
| `error` | Servidor → agent | Ante un rechazo | Código de una lista cerrada, texto para el log y el ID del mensaje rechazado |

La credencial de `enroll_ack` viaja **una sola vez**: el control plane guarda solo su hash y el
agent la escribe con permisos `0600`. Si se pierde, hay que enrolar de nuevo.

Los códigos de error son `invalid_token`, `expired_token`, `unauthorized`,
`unsupported_version`, `invalid_message`, `rate_limited` e `internal`. El agent decide qué hacer
mirando el código, nunca el texto: `Retryable()` dice cuáles tiene sentido reintentar.

## Métricas: un lote en vuelo

El agent manda un lote de `metrics` y espera la respuesta antes de mandar el siguiente. Por eso
`metrics_ack` no dice a qué lote responde: alcanza con `last_ts`, la marca de tiempo de la última
muestra guardada. El agent suelta de su buffer lo que ya mandó hasta esa marca.

| Respuesta del control plane | Qué hace el agent |
| --- | --- |
| `metrics_ack` | Suelta lo confirmado. Lo que quedó del lote sin confirmar sale en el próximo. |
| `error` con `in_reply_to` del lote, reintentable | Vuelve a mandar esas muestras en el lote siguiente. |
| `error` con `in_reply_to` del lote, definitivo | Las descarta: el control plane no las va a aceptar nunca. |
| Nada en 30 s, o se corta el canal | Da el lote por perdido y lo vuelve a mandar. |

Como un lote puede llegar dos veces, **el control plane guarda las muestras de forma
idempotente**: una muestra repetida (mismo servidor y misma marca de tiempo) no se duplica. El
agent trunca las marcas de tiempo al milisegundo para que sobrevivan intactas el viaje por
PostgreSQL, que guarda microsegundos.

**Tamaño:** ningún mensaje del agent pasa de `MaxMessageBytes` (256 KiB). El control plane corta
el canal si recibe algo mayor, y el agent parte los lotes grandes para no pasarse. La muestra más
grande que acepta el contrato ocupa unos 112 KB, así que siempre entra sola.

**Lo que además revisa el control plane**, porque el contrato no puede saberlo:

- **La marca de tiempo:** entre las 48 h que se guardan y 10 minutos en el futuro. Fuera de esa
  ventana el reloj del servidor está mal, y reenviar no lo arregla: el lote se rechaza con
  `invalid_message` y el agent lo descarta.
- **Que los valores entren en las columnas de PostgreSQL:** bytes hasta 2⁶³ − 1, por ejemplo.
- **La frecuencia:** hasta 30 lotes seguidos y después uno por segundo, por canal. Pasado eso
  contesta `rate_limited`, que es reintentable: el agent no pierde las muestras.

## Estado de runtime: una foto entera

`runtime_state` es la foto de lo que corre en el servidor: contenedores de Docker, servicios de
systemd y los procesos que no pertenecen a ninguno de los dos. Viaja **entera**, nunca como
diferencia: si una se pierde, la siguiente la reemplaza y no hay nada que reconstruir.

`taken_at` identifica la foto. El control plane descarta una más vieja que la que ya tiene, y
`runtime_state_ack` confirma por esa misma marca. Como en las métricas, hay una sola foto en vuelo.

**Cada fuente dice si se pudo leer**, y eso decide qué hace el control plane con su lista:

| `status` de la fuente | Significado | Qué hace el control plane |
| --- | --- | --- |
| `ok` | Se leyó entera | La lista es la verdad: lo que no aparece, dejó de existir. |
| `unavailable` | No existe en este servidor (un VPS sin Docker) | No hay nada que mostrar de esa fuente. |
| `error` | Existe pero falló la lectura | **Conserva lo último que supo.** Un Docker que no contesta no borra los contenedores del panel. |

Una fuente que no está en `ok` no puede traer elementos: no se sabría si son la lista completa.

**Nombres con los caracteres de cada sistema.** Los IDs de contenedor son 64 caracteres
hexadecimales; los nombres, los de Docker (`[a-zA-Z0-9_.-]`); las unidades, los de systemd y
terminadas en `.service`. Estos nombres llegan al panel y, más adelante, a los parámetros de los
comandos, así que cualquier otro carácter se rechaza. Los estados de contenedor y de servicio son
listas cerradas con un valor `unknown`: el adaptador traduce ahí un estado que no conoce, para que
una versión nueva de Docker o de systemd no haga rechazar la foto entera.

**La línea de comando de un proceso sale enmascarada del agent** (ADR-011): una contraseña pasada
como argumento no deja el servidor. Además viene recortada y sin caracteres de control.

**Tamaño:** los topes por tipo son 200 contenedores, 300 servicios y 20 procesos, pero lo que manda
es `MaxMessageBytes`: una foto no se puede partir. Un servidor con todos los topes al máximo y
nombres de largo realista ocupa unos 155 KB, así que en la práctica entra siempre. Si alguna vez no
entrara, el agent deja afuera lo menos importante y lo cuenta en `omitted`.

## Comandos: una lista cerrada

`command` le pide al agent una **operación de una lista cerrada** (regla 2 del proyecto). No
existe una operación que ejecute texto libre: cada una lleva sus parámetros en su propio campo,
tipados y validados. En el Sprint 2 la lista tiene una sola, de lectura:

| Operación | Parámetros | Qué hace |
| --- | --- | --- |
| `get_logs` | `kind` (`container` o `service`), `target` (ID completo del contenedor o nombre de la unidad) y `lines` (1 a 1000) | Los últimos renglones del log, del más viejo al más nuevo |

El ID del envelope es el ID del comando. El agent lo usa para no ejecutar dos veces un comando
repetido (devuelve la misma respuesta) y lo devuelve en `command_ack.command_id`.

**Lo que además revisa el agent**, aunque el comando pase el contrato:

- **Que el objetivo exista en su última foto de runtime.** Un contenedor o servicio con un nombre
  válido pero que el agent no ve se rechaza con `unknown_target`.
- **Que no haya más de dos comandos corriendo.** Si los hay, contesta `busy` y el control plane
  reintenta más tarde.

**La respuesta**, `command_ack`, trae el estado (`ok`, `rejected` o `failed`), un código de una lista
cerrada si no terminó bien (`unknown_operation`, `invalid_params`, `unknown_target`, `busy`,
`unavailable`, `timeout`, `internal`) y, si terminó bien, el resultado. Los renglones de log salen del
agent **sin secuencias de escape de terminal, con los secretos enmascarados y recién después
recortados** a 2048 bytes (ADR-011). Si mil renglones no entran en un mensaje, se dejan afuera los
más viejos y se marca `truncated`.

Las operaciones largas de las fases siguientes (un deploy) van a contestar igual al terminar, e
informar su avance aparte con `deploy_event`.

## Reglas de versionado

- Agregar un campo opcional **no** cambia `v`. Quitar un campo o cambiar su significado, **sí**.
- Los campos desconocidos se ignoran al leer, para que un agent viejo tolere un control plane nuevo.
- Una versión que el receptor no conoce se rechaza con `unsupported_version`; no se adivina.
- Todo mensaje se valida al entrar: los límites de tamaño están para acotar lo que llega de la red.

## Cambiar el contrato

Los archivos de `testdata/` fijan el formato exacto que viaja por el cable. Si un cambio los
mueve, el test falla y el diff muestra qué se modificó, que es justo lo que hay que revisar con
cuidado porque del otro lado puede haber un agent viejo.

```bash
go test ./proto/ -run TestGolden -update
```

Los valores de esos archivos son de ejemplo: no hay ningún token ni credencial real.
