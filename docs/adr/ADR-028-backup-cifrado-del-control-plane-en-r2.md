# ADR-028: Backup cifrado del control plane con restic en Cloudflare R2

- **Estado:** aceptada
- **Fecha:** 2026-11-14

## Contexto

Hasta el cierre del MVP, el único respaldo del control plane era un `pg_dump` a mano en `/root` de
`srv936263`, antes de cada actualización. Si se perdía el disco, se perdían la base y la clave que
cifra los secretos TOTP ([ADR-013](ADR-013-segundo-factor-totp.md)). Sin esa clave, ningún usuario
puede volver a entrar con su segundo factor. La auditoría del 2026-11-11 lo puso entre lo primero a
resolver, y Juan quiere pausar el desarrollo con esto resuelto.

Son decenas de MB por día. El servidor usa el Docker de snap, que solo monta archivos de `/root` y
de `/var/snap/docker/common` ([ADR-018](ADR-018-control-plane-en-servidor-propio-compartido.md)).

## Decisión

**Una copia diaria, cifrada con restic, en un bucket de Cloudflare R2.** La hace un servicio
`backup` del compose de producción, con su script adentro de la imagen, como el cotizador.

- **Qué entra:** el volcado de la base (`pg_dump -Fc` sin comprimir, para que restic comprima y
  deduplique), `secrets/totp.key` y `.env`.
- **Retención:** 7 copias diarias, 4 semanales y 6 mensuales. En cada copia, `restic check` lee el
  10% de los datos.
- **El contenedor no tiene capacidades**, salvo `DAC_READ_SEARCH` para leer la clave TOTP, que es
  del usuario del control plane. Corre con la raíz de solo lectura.
- **La contraseña del repositorio** se genera en el servidor y se guarda además en el gestor de
  contraseñas de Juan. El token de R2 solo puede tocar ese bucket.
- **Restore probado:** `deploy/backup/ensayo-restore.sh` levanta una base y un control plane
  descartables sobre una copia y espera a `/readyz`. El CI lo corre de punta a punta en cada push a
  `main` (`deploy/e2e/backup.sh`).

**Por qué R2:** Juan ya usa Cloudflare, y con este volumen entra en el plan gratis (10 GB). No
cobra por bajar datos, así que restaurar o ensayar no cuesta. Es compatible con S3, y restic lo
habla sin nada extra.

**Por qué restic:** cifra antes de subir, deduplica entre días, aplica la retención y revisa la
integridad. Es un solo binario que está en Alpine.

## Alternativas

- **Backblaze B2.** Igual de barato y compatible con S3, pero es una cuenta más en otro proveedor.
- **Hetzner Storage Box, por SFTP.** Desde unos EUR 4 por mes, y una llave SSH adentro del
  contenedor en lugar de un token limitado a un bucket.
- **Otro servidor de Juan.** Gratis, pero la copia quedaría en un servidor que también administra
  status-sv: un mismo problema podría llevarse el original y la copia.
- **Volcados con fecha en el disco**, como el cotizador. No sirven: se pierden con el disco.

## Consecuencias

**Positivas**

- Perder `srv936263` deja de ser perder el control plane: con la copia, la contraseña y el runbook,
  se levanta en otro servidor.
- El restore deja de ser una suposición: lo prueba el CI, y el ensayo corre en el servidor sin
  tocar lo que está andando.

**Negativas, aceptadas a sabiendas**

- **La contraseña del repositorio es un punto único.** Si se pierde la del gestor y el servidor a
  la vez, las copias no se pueden abrir.
- **La copia está tan segura como la cuenta de Cloudflare.** El token alcanza solo el bucket, pero
  quien tome la cuenta puede borrar las copias. Leerlas no puede, porque restic las cifra y sin la
  contraseña no se abren. Si hiciera falta, R2 tiene reglas de bloqueo de objetos.
- **Si el servicio no corre, nadie se entera solo.** El contenedor queda `unhealthy`, pero en
  `srv936263` no hay un agent que lo mire. `STATUSSV_BACKUP_PING_URL` lo resuelve con un monitor
  externo, que es el siguiente pendiente de la auditoría.

## Cuándo revisarla

- Si la base supera unos pocos GB, o el plan gratis de R2 cambia.
- Si el control plane se muda: el destino sigue sirviendo, pero el ensayo hay que correrlo en el
  servidor nuevo.
