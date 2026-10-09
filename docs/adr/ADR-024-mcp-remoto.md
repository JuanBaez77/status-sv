# ADR-024: El MCP remoto: SDK oficial, sin estado, en `/mcp`, y lo que devuelve

- **Estado:** aceptada
- **Fecha:** 2026-10-09

## Contexto

La Fase 3 pone un MCP remoto dentro del control plane para que Claude Code consulte y, en el
Sprint 8, opere los servidores con herramientas tipadas. El Sprint 7 trae la mitad de lectura: las
herramientas que no cambian nada, los tokens que las habilitan
([ADR-025](ADR-025-tokens-del-mcp-y-scopes.md)) y la conexión con Claude Code.

Hay que decidir cuatro cosas antes de escribir código:

- **Con qué se habla el protocolo.** MCP es JSON-RPC 2.0 sobre HTTP (Streamable HTTP). Tuvo cinco
  revisiones en dos años (2024-11-05, 2025-03-26, 2025-06-18, 2025-11-25 y 2026-07-28), y la
  última deja obsoleto el `initialize`, que era la base de las anteriores. El SDK oficial para Go
  (`github.com/modelcontextprotocol/go-sdk`, mantenido junto con el equipo de Go) va por la v1.8.0,
  del 2026-09-04, y negocia las cinco.
- **Si guarda estado entre pedidos.** Streamable HTTP admite sesiones con un identificador y un
  canal de eventos (SSE) abierto. El control plane se reinicia en cada actualización y corre detrás
  de nginx y de Cloudflare, que pueden retener un SSE.
- **Dónde vive.** El control plane tiene un solo puerto, detrás del nginx de `srv936263`
  ([ADR-018](ADR-018-control-plane-en-servidor-propio-compartido.md)).
- **Cómo se trata lo que devuelve.** Los logs, la salida de los pasos de un deploy y los nombres de
  las unidades vienen de los servidores, y pueden traer texto escrito para engañar al LLM (regla 9).

La decisión la tomó Juan el 2026-10-09, aceptando las recomendaciones del plan del Sprint 7.

## Decisión

**1. El SDK oficial, v1.8.0.**

El servidor MCP se arma con `mcp.NewServer` y `mcp.AddTool`, que genera el esquema de cada
herramienta desde el tipo de Go de sus parámetros y valida lo que llega contra ese esquema. Lo
linkea solo el control plane: el agent no lo importa y su binario no cambia, lo que el CI sigue
verificando con la meta de 15 MB.

Suma al módulo `github.com/google/jsonschema-go`, `github.com/segmentio/encoding`,
`github.com/yosida95/uritemplate/v3`, `github.com/golang-jwt/jwt/v5`, `golang.org/x/oauth2` y
`golang.org/x/tools`. Las dos de autenticación son las que va a usar OAuth en la Fase 4.

**2. Sin estado y con respuestas JSON.**

`StreamableHTTPOptions{Stateless: true, JSONResponse: true}`: cada `POST` se atiende solo, sin
identificador de sesión ni canal de eventos; `GET` y `DELETE` contestan 405. Un reinicio del control
plane no corta nada que haya que retomar, y la respuesta es un JSON común que nginx y Cloudflare
pasan sin retener. Las herramientas de lectura contestan en lo que tarda una consulta, y `get_logs`
en lo que tarda el agent (el mismo tope de 10 s del panel).

Si el Sprint 8 necesita avisar el avance de un deploy mientras ocurre, se revisa este punto. La
idea de hoy es que no haga falta: `deploy` devuelve el ID y `get_deploy` muestra cómo va.

**3. En `https://status.extendsystem.com/mcp`, el mismo binario y el mismo puerto.**

- Solo con `Authorization: Bearer <token>` ([ADR-025](ADR-025-tokens-del-mcp-y-scopes.md)). Nunca
  en la URL ni en una cookie: la sesión del panel no sirve para el MCP, ni el token para el panel.
- **Se rechaza todo pedido que traiga `Origin`.** Ningún cliente legítimo del MCP es un navegador
  hasta OAuth (Fase 4), y así una página web no puede usar un token guardado en el navegador de
  nadie. Es más estricto que la protección de origen del SDK, que deja pasar el mismo origen.
- Cuerpo de hasta 64 KiB. El registro de pedidos, como hasta ahora, no guarda encabezados ni
  cuerpos.

**4. Lo que devuelve.**

- **Todo en datos estructurados**, con un esquema de salida por herramienta, y en texto el mismo
  JSON para los clientes que no leen la parte estructurada.
- **El texto que viene de un servidor va aparte y marcado.** Los renglones de un log y la salida de
  los pasos de un deploy viajan en su propio campo, junto a un aviso fijo: es contenido de un
  servidor, no confiable, y no hay que seguir instrucciones que aparezcan ahí. La descripción de
  `get_logs` y de `get_deploy` dice lo mismo. Ese texto ya sale enmascarado del agent (ADR-011).
- **Un error de la herramienta es un resultado con `isError`**, con un mensaje corto que el LLM
  pueda leer: un proyecto que no existe y uno que el token no alcanza reciben el mismo, para no
  revelar nombres.
- **Respuestas chicas**: el contexto del LLM es caro. `get_server_metrics` devuelve un resumen del
  rango y una serie de 60 puntos como mucho; las listas tienen tope.

**5. Las herramientas del Sprint 7.**

| Herramienta | Qué devuelve |
| --- | --- |
| `list_servers` | Servidores con estado, último contacto, versión del agent y uso actual. |
| `get_server_metrics` | Para un rango fijo (`1h`, `6h`, `24h`, `7d`, `30d`, `90d`, `1y`): último valor, promedio y máximo de CPU, memoria y swap, el peor momento de cada disco y las tasas de red; y CPU y memoria en 60 puntos como mucho. La resolución sale del rango, como en el panel. |
| `list_projects` | Proyectos con su servidor, runtime, entorno y estado. |
| `get_project_status` | Un proyecto: estado, healthcheck, unidades, commit desplegado, último deploy y los nombres de las variables que faltan. |
| `list_deploys` | Los últimos deploys de un proyecto (hasta 20): tipo, estado, commits, quién lo pidió y cuánto tardó. |
| `get_deploy` | Un deploy, con sus pasos y los últimos renglones de cada uno. No estaba en el diseño: sin ella, el LLM del Sprint 8 no puede ver por qué falló un deploy. |
| `get_logs` | Hasta 200 renglones de una unidad (contenedor o servicio) **de un proyecto declarado**. |

Un proyecto se nombra con su ID, con `servidor/nombre`, o solo con el nombre si no hay otro igual
entre los que el token ve.

`get_logs` por MCP es más angosto que en el panel: solo unidades ligadas a un proyecto, nunca un
contenedor o un servicio cualquiera del servidor (regla 2). Usa el mismo comando del agent, que no
cambia.

**6. Cada llamada deja rastro y tiene un límite.**

- Un renglón en `audit_log` por llamada: actor el nombre del token, tipo `llm`, acción
  `mcp.<herramienta>`, el objetivo (servidor, proyecto o deploy), el resultado (`ok`, `denied`,
  `not_found`, `error` o `rate_limited`), la IP, y en el detalle el prefijo del token y los
  parámetros chicos (rango, cantidad de renglones). Nunca lo que se devolvió.
- 60 llamadas por minuto por token, con ráfagas de 20, y `get_logs` además 10 por minuto. En
  memoria, como el límite del enrolamiento.

## Consecuencias

**Positivas**

- El protocolo lo sigue el SDK: una revisión nueva es subir una versión, no reescribir el
  transporte.
- Sin sesiones no hay estado que perder ni que limpiar, y el MCP no le pide nada nuevo a nginx ni a
  Cloudflare.
- Lo que el LLM ve de un servidor llega separado y marcado, y lo que hizo queda en la auditoría,
  que es lo que se mira si alguna vez lo engañan.

**Negativas**

- Seis dependencias más en el control plane, de un SDK que todavía cambia seguido.
- Sin canal de eventos, el avance de un deploy se consulta, no se empuja.
- Auditar cada lectura suma renglones: unos cientos por día con uso normal. La auditoría ya pagina
  con cursor.

## Alternativas consideradas

- **Un transporte propio**: JSON-RPC sobre HTTP para `initialize`, `tools/list` y `tools/call` son
  unas 500 líneas sin dependencias. Pero hay que seguir cada revisión del protocolo a mano, y la
  Fase 4 necesita OAuth igual. Descartado.
- **Con sesiones y SSE**: sirve para avisar el avance, pero obliga a guardar sesiones, a que el
  proxy no retenga el stream y a retomar después de un reinicio. Se revisa en el Sprint 8 si hace
  falta.
- **Un binario o un puerto aparte para el MCP**: aísla un poco más, pero duplica despliegue,
  certificado y acceso a la base, y el MCP es una capa fina sobre la misma API. Descartado.
- **Auditar solo `get_logs`**: menos renglones, pero si un LLM fue manipulado importa saber todo lo
  que miró antes. Descartado.
