# ADR-010: Usuarios del panel con argon2id y sesiones en la base

- **Estado:** aceptada
- **Fecha:** 2026-09-22

## Contexto

El panel necesita login desde el Sprint 1. Más adelante, desde ese mismo panel se van a aprobar
deploys a producción (Fase 3), así que una sesión robada vale mucho. El diseño pide argon2id para
las contraseñas y TOTP, que llega en el Sprint 3; mientras tanto el panel queda además detrás de
Cloudflare Access, que configura Juan.

Hay que decidir cómo se guardan las contraseñas y las sesiones, cuánto dura una sesión, cómo se
frena a quien prueba contraseñas y cómo se crea el primer usuario sin que su contraseña pase por
ningún lado.

## Decisión

**Contraseñas**

- argon2id con la primera configuración que recomienda OWASP: 19 MiB, 2 pasadas, 1 hilo. Se guarda
  en formato PHC, que lleva los parámetros: si se suben, los hashes viejos se siguen leyendo y se
  actualizan solos en el siguiente login.
- Como máximo dos hashes a la vez. Cada uno usa 19 MiB y la meta del control plane es 100 MB.
- De 12 a 128 caracteres, sin reglas de composición (NIST SP 800-63B).
- Un usuario inexistente se verifica contra un hash de relleno: tarda lo mismo que una contraseña
  equivocada y responde lo mismo, así el login no dice qué nombres existen.

**Sesiones**

- Un token de 32 bytes al azar (`svses_…`) que viaja en la cookie `__Host-statussv_session`, con
  `HttpOnly`, `Secure`, `SameSite=Strict` y `Path=/`. En la base se guarda solo su SHA-256, como las
  credenciales de los agents (ADR-006): un token al azar no necesita un hash lento.
- Vence a las **24 h sin uso** o a los **7 días de abierta**, lo que llegue primero, por decisión
  de Juan. El uso se anota como mucho una vez por minuto, para no escribir en cada pedido.
- Cambiar la contraseña cierra todas las sesiones del usuario, en la misma transacción.
- Un barrido cada hora borra las vencidas; una sesión vencida ya no sirve aunque siga en la tabla.

**Pedidos de otro sitio (CSRF)**

- `SameSite=Strict` no manda la cookie en pedidos que arrancan en otro sitio.
- Además, las rutas del panel que cambian algo pasan por `http.CrossOriginProtection` de Go, que
  rechaza lo que llega con `Sec-Fetch-Site` u `Origin` de otro sitio. Las rutas de los agents quedan
  afuera: no son de un navegador.

**Intentos**

- 5 logins por minuto por IP, además del costo de argon2id.

**Primer usuario**

- Lo crea una persona por SSH con `server user-add --username NOMBRE`, que pide la contraseña en la
  terminal sin eco. Nunca por argumento ni por variable de entorno. Claude no la ve.

## Consecuencias

**Positivas**

- Si se filtra la base, no sirve para entrar: hay hashes argon2id y SHA-256 de tokens al azar.
- Una sesión se puede cerrar al instante desde el servidor, cosa que un JWT no permite.
- El JavaScript del panel nunca ve el token.

**Negativas**

- Cada pedido del panel hace una lectura en la base para validar la sesión. Para una persona es
  nada; si hiciera falta, se agrega un caché corto en memoria.
- El límite por IP no frena a alguien con muchas IPs. Lo cubren argon2id, el tope de hashes
  simultáneos y Cloudflare Access; TOTP (Sprint 3) cierra el resto.
- La cookie `Secure` exige HTTPS. En desarrollo los navegadores la aceptan igual en `localhost`.
- Los inicios de sesión quedan en el registro, no en `audit_log`, que todavía no existe. Cuando se
  cree (Fase 2), el login pasa a registrarse ahí también.

## Alternativas consideradas

- **JWT sin estado:** no se puede revocar una sesión sin armar una lista de revocados, que es volver
  a tener estado. Descartado.
- **bcrypt:** trunca a 72 bytes y no es resistente a GPU como argon2id. El diseño ya pedía argon2id.
- **Bloquear la cuenta tras N intentos:** le permitiría a cualquiera bloquear a Juan fuera de su
  panel. Se prefiere el límite por IP y el costo del hash.
- **Contraseña del primer usuario por variable de entorno:** quedaría en el `.env`, en el historial
  y en el entorno del proceso. Descartado.
