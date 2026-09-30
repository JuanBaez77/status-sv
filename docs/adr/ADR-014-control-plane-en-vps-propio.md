# ADR-014: El control plane vive en un VPS propio, detrás de Caddy

- **Estado:** aceptada
- **Fecha:** 2026-09-30

## Contexto

[ADR-007](ADR-007-control-plane-en-servidor-de-cliente.md) dejó el control plane en
`cristales-baez-prod`, la producción de un cliente, detrás de su nginx. Aceptó a sabiendas que un
monitor no puede avisar de la caída del servidor donde vive y que comparte el radio de impacto con
el cliente, y fijó cuándo revisarlo: antes de un segundo cliente con agent, de las alertas o de las
herramientas de escritura.

El Sprint 3 suma el segundo servidor y el Sprint 4 empieza la escritura (deploy). El 2026-09-28 Juan
decidió mover el control plane a un VPS propio en este sprint.

## Decisión

**El control plane corre en un VPS dedicado, sin nada más, con el perfil `caddy` del compose de
producción.** Es la opción A de ADR-007, que quedó como destino.

- **Caddy toma los puertos 80 y 443** y pide el certificado a Let's Encrypt. El dominio sigue siendo
  el mismo y Cloudflare sigue adelante con el proxy activado, en modo SSL *Full (strict)*.
- **El primer certificado se pide con el proxy de Cloudflare apagado** (registro en "DNS only"), y se
  prende después. Las renovaciones pasan a través del proxy: Let's Encrypt sigue la redirección a
  HTTPS y Caddy contesta el desafío con el certificado vigente.
- **La IP real del cliente** sale de `CF-Connecting-IP`, y Caddy la cree solo si la conexión viene
  de un rango oficial de Cloudflare. Al control plane le pasa un único valor en `X-Forwarded-For`,
  así el límite de intentos del login y del enrolamiento sigue siendo por cliente y no por nodo de
  Cloudflare. Es lo mismo que hacía el `realip` de nginx.
- **Caddy comprime** (zstd y gzip) el panel y la API, como lo hacía nginx.
- **Los datos se mudan, no se empieza de cero:** un volcado de la base del servidor viejo se
  restaura en el nuevo antes de aplicar las migraciones pendientes. Los servidores, las
  credenciales de los agents y la historia de métricas se conservan, y como el dominio no cambia,
  los agents reconectan solos cuando el DNS apunta al VPS nuevo: no se reenrolan ni se tocan.
- **El corte dura menos de 10 minutos**, que es lo que el agent guarda de métricas en memoria
  mientras no tiene conexión. Pasado eso, se pierden muestras (no el servidor ni su credencial).
- `cristales-baez-prod` queda solo con su agent. Se quita el sitio de nginx de status-sv y los
  contenedores; el volumen de la base se conserva una semana como respaldo y después se borra.

## Consecuencias

**Positivas**

- Se cierran los riesgos que ADR-007 aceptó: el control plane ya no cae con el servidor de un
  cliente, y comprometer uno no compromete al otro.
- Los datos de un segundo cliente no viven en el servidor del primero.
- Se deja de tocar el nginx de producción de un cliente en cada actualización.

**Negativas**

- Un VPS más que pagar y mantener (actualizaciones del sistema, firewall, backups).
- Hay que acordarse de apagar el proxy de Cloudflare si alguna vez se pierde el volumen `caddy-data`
  y hay que pedir un certificado desde cero.
- Los rangos de Cloudflare quedan escritos en el Caddyfile: si Cloudflare los cambia, hay que
  actualizarlos, como ya pasaba con nginx.

## Alternativas consideradas

- **Seguir en el servidor del cliente:** contradice lo que ADR-007 fijó para este momento.
- **Empezar con una base vacía en el VPS nuevo:** más simple, pero pierde la historia y obliga a
  reenrolar cada agent con un token nuevo por SSH. Descartada.
- **Certificado por desafío DNS con la API de Cloudflare:** evita apagar el proxy la primera vez,
  pero necesita una imagen de Caddy con un módulo extra y un token de Cloudflare guardado en el
  servidor. No compensa para un paso que se hace una vez.
- **Cerrar los puertos 80 y 443 a todo lo que no sea Cloudflare:** esconde el VPS del todo, pero
  complica pedir el primer certificado. Queda como endurecimiento posible, anotado en el runbook.
