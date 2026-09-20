# ADR-006: Autenticación de agents con token de un solo uso y credencial propia

- **Estado:** aceptada
- **Fecha:** 2026-09-20

## Contexto

El documento de diseño define el enrolamiento en dos etapas —un token de un solo uso que vence a
los 15 minutos y después una credencial propia por agent, revocable— pero no dice cómo se
implementa cada pieza. Hay cuatro decisiones concretas que tomar antes de escribir el código del
control plane y del agent:

1. Por dónde viaja el enrolamiento: ¿un pedido HTTP aparte o el primer mensaje del WebSocket?
2. Cómo se autentica el WebSocket una vez que el agent tiene su credencial.
3. Qué forma tienen los secretos.
4. Con qué se hashean, sabiendo que `users` va a usar argon2id para contraseñas de personas.

## Decisión

**1. El enrolamiento es un `POST /v1/agents/enroll` por HTTP, y el WebSocket se abre después.**

Es la secuencia que muestra el diagrama del diseño: el agent presenta el token, recibe su
credencial y recién entonces abre el canal ya autenticado. Separarlo tiene una ventaja práctica:
el enrolamiento es un pedido único, con su límite de frecuencia y su respuesta de error clara,
sin mezclarse con la máquina de estados del canal permanente.

El cuerpo del pedido y la respuesta usan **el mismo envelope versionado** que el WebSocket
(ADR-003), así hay una sola forma de hablar y el versionado vale también para el enrolamiento.

**2. El WebSocket autentica con `Authorization: Bearer <credencial>`.**

Nunca en la URL: las query strings quedan en los logs del proxy, en el historial y en las
métricas. Va en el encabezado, que el middleware de registro no escribe.

**3. Los secretos son 32 bytes de `crypto/rand` en base64url sin relleno, con prefijo.**

`svenr_` para tokens de enrolamiento y `svagt_` para credenciales de agent. El prefijo hace
evidente qué es cada cosa si aparece en un archivo de configuración, y permite rechazar temprano
una credencial usada donde va un token, sin consultar la base.

**4. Se guarda el SHA-256, no argon2id.**

Estos secretos son 256 bits al azar, no contraseñas elegidas por una persona: no hay diccionario
ni fuerza bruta que sirva, así que un hash lento no agrega seguridad. Y sí tiene un costo real: la
credencial se verifica en cada reconexión de cada agent, y argon2id está diseñado para tardar.
argon2id queda para `users`, donde la entropía la elige una persona.

La comparación es en tiempo constante, y el tipo `auth.Secret` no se puede imprimir por accidente:
`String` y `LogValue` devuelven una marca, y hay que pedir `Reveal()` a propósito.

## Consecuencias

**Positivas**

- Un agent comprometido se corta revocando su credencial, sin tocar a los demás.
- Nada reutilizable viaja dos veces: el token muere al usarse y la credencial se manda una vez.
- Si la base se filtra, no hay secretos que sirvan: solo hashes de valores irrecuperables.
- Un secreto que llegue por descuido a un log aparece como `[secreto oculto]`, y hay un test que
  lo verifica.

**Negativas**

- Si el agent pierde su credencial, hay que enrolarlo de nuevo: no hay forma de recuperarla.
- SHA-256 sin sal significa que dos agents con la misma credencial tendrían el mismo hash. No es
  un problema acá porque las credenciales las genera el servidor y son únicas por construcción,
  pero es la razón por la que este esquema **no** sirve para contraseñas.
- El enrolamiento por HTTP obliga al agent a hablar dos protocolos, HTTP y WebSocket, aunque
  contra el mismo servidor y con el mismo formato de mensajes.

## Alternativas consideradas

- **Enrolar dentro del WebSocket, como primer mensaje:** un solo camino de código, pero mezcla el
  alta con el canal permanente y complica aplicar un límite de frecuencia distinto al enrolamiento,
  que es el que más hay que proteger. Descartado.
- **Credencial en la query string del WebSocket:** más simple de escribir en algunos clientes, pero
  deja el secreto en los logs del proxy. Descartado.
- **argon2id para las credenciales de agent:** más lento en cada reconexión sin ganar nada frente a
  256 bits al azar. Descartado, con el criterio explícito de que sí se usa para contraseñas.
- **JWT firmados en lugar de credenciales opacas:** evitarían una consulta a la base por conexión,
  pero revocar exigiría una lista de revocación, que es justamente la consulta que se quería
  evitar. Para una conexión larga, la consulta ocurre una vez cada varias horas. Descartado.
- **mTLS:** es la meta declarada para la Fase 4 y no cambia nada de lo de acá: el enrolamiento
  seguiría siendo el momento en que se emite el material de cliente.
