# ADR-013: Segundo factor TOTP obligatorio en el panel

- **Estado:** aceptada
- **Fecha:** 2026-09-28

## Contexto

Desde la Fase 3 el panel va a aprobar deploys a producción, y el diseño pide TOTP obligatorio para
las cuentas que pueden aprobar. ADR-010 dejó el TOTP para el Sprint 3 y, mientras tanto, el panel
queda detrás de Cloudflare Access. Con un solo tipo de usuario (ADR-005), todas las cuentas van a
poder aprobar.

Hay que decidir cómo se da de alta, cómo se guarda el secreto, qué pasa si se pierde el teléfono y
qué librería se usa.

## Decisión

Todo esto lo aprobó Juan el 2026-09-28.

**1. TOTP estándar y obligatorio para todo usuario.**

- RFC 6238: HMAC-SHA1, 6 dígitos, pasos de 30 s, con una tolerancia de un paso para adelante y otro
  para atrás. Es lo que entienden todas las apps de autenticación.
- El secreto son 20 bytes al azar.
- Un código no se acepta dos veces: se guarda el último paso aceptado y se rechaza cualquier código
  de ese paso o de uno anterior.
- No hay "recordar este dispositivo". La sesión ya vence a los 7 días (ADR-010).

**2. El login pasa a tener dos pasos, y la sesión, una etapa.**

- Una contraseña correcta abre una sesión **a medias**, que solo sirve para el paso siguiente y para
  cerrar sesión. Vence a los 5 minutos y admite 5 intentos de código, además del límite por IP
  de ADR-010.
- Si el usuario ya tiene TOTP, la sesión a medias espera el código. Si todavía no lo tiene, solo
  permite darlo de alta.
- Con el código correcto, la sesión a medias se cierra y se abre una completa con un token nuevo.
  El token de la etapa anterior no sirve para nada más.
- Las rutas del panel y el WebSocket exigen una sesión completa.

**3. El alta se hace desde el panel y se confirma con un código.**

- El servidor genera el secreto y lo guarda como pendiente. El panel lo muestra como QR y como texto,
  para cargarlo a mano.
- Recién al confirmar con un código válido el TOTP queda activo. Se generan entonces **10 códigos de
  recuperación**, que se muestran una sola vez.
- El QR se arma en el servidor con `rsc.io/qr` (una librería chica, sin dependencias) y llega como
  imagen. El TOTP en sí se escribe con la biblioteca estándar (`crypto/hmac`): son pocas líneas y
  se prueban contra los vectores de la RFC.

**4. El secreto se guarda cifrado, con la clave fuera de la base.**

- AES-256-GCM, con el ID del usuario como dato asociado: un secreto copiado a otra fila no descifra.
- La clave son 32 bytes al azar en un archivo que se monta en el contenedor como secret de Docker
  Compose. El control plane lo lee de `STATUSSV_TOTP_KEY_FILE` y no arranca si falta o si no mide
  32 bytes.
- Un volcado de la base, sin ese archivo, no sirve para generar códigos.

**5. Códigos de recuperación y reset.**

- Cada código tiene 80 bits al azar, escritos como 16 caracteres en base32 en grupos de cuatro. Se
  guarda su SHA-256: con 80 bits no hace falta un hash lento, igual que con los tokens (ADR-006).
- Cada código sirve una vez y abre una sesión completa. El panel muestra cuántos quedan.
- Si se pierden el teléfono y los códigos, una persona con SSH corre
  `server user-totp-reset --username NOMBRE`. Eso borra el secreto y los códigos y cierra todas las
  sesiones del usuario; el próximo login vuelve a pedir el alta.

## Consecuencias

**Positivas**

- Una contraseña filtrada ya no alcanza para entrar ni, más adelante, para aprobar un deploy.
- Si se filtra la base, no se pueden generar códigos: haría falta también el archivo de la clave.
- El reset exige acceso al servidor, que es la misma barrera que el alta del primer usuario.

**Negativas**

- Hay una clave más que resguardar. Si se pierde el archivo, todos los usuarios tienen que hacer
  el reset y dar de alta el TOTP de nuevo. Se documenta en el runbook y entra en los backups.
- El primer login después de desplegar obliga al alta. Se avisa antes de desplegar.
- `rsc.io/qr` suma una dependencia al control plane (no al agent).

## Alternativas consideradas

- **`pquerna/otp`:** trae su propia librería de códigos de barras y más de lo que hace falta.
  Descartada.
- **Alta por SSH, con el QR en la terminal:** el paso por el panel es igual de seguro, porque exige
  la contraseña y confirmar con un código, y es más cómodo. Descartada.
- **WebAuthn o passkeys:** más fuerte contra el phishing, pero mucho más código en el servidor y en
  el front. Queda en "más adelante".
- **Secreto en claro en la base:** un volcado alcanzaría para generar códigos. Descartado.
