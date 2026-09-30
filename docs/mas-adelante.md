# Más adelante

Ideas que **no** entran en el alcance actual (Fases 1 a 3). Se anotan acá para no perderlas y
para no dejarlas colarse en un sprint. Nada de esta lista se implementa sin antes moverlo al
roadmap de [DISENO.md](DISENO.md).

## Fuera del MVP por decisión explícita

- Multiusuario, organizaciones, roles finos y facturación (ver [adr/ADR-005-alcance-single-tenant.md](adr/ADR-005-alcance-single-tenant.md)).
- Agents para Windows o macOS.
- Orquestación tipo Kubernetes y autoscaling.
- Edición libre de archivos de configuración desde el panel (solo las plantillas del proxy son editables).

## Planificado para la Fase 4

- Worker de smoke tests con Playwright.
- Alertas por correo o Telegram.
- Chequeos de seguridad del servidor: intentos fallidos de SSH, puertos abiertos, actualizaciones
  pendientes, expiración de certificados, estado de backups, contenedores corriendo como root.
- mTLS entre agent y control plane.
- OAuth 2.1 con registro dinámico de clientes, para conectar el MCP a claude.ai.
- Actualización del agent desde el panel, con verificación de checksum y firma.

## Ideas sin fecha

- Gestión de backups de los proyectos alojados y restore probado.
- Renovación y estado de certificados como vista propia del panel.
- Comparación entre entornos (qué versión corre en staging y cuál en producción).
- Plantillas de proyecto para acelerar el alta de un cliente nuevo.
- Notificaciones de deploy a clientes.
- Modo staging automático por cada PR.
- Aprobaciones por enlace firmado desde el celular, además del panel.
- WebAuthn o passkeys como segundo factor, además de TOTP (ver
  [adr/ADR-013-segundo-factor-totp.md](adr/ADR-013-segundo-factor-totp.md)).
- TimescaleDB, si el particionado nativo deja de alcanzar (ver [adr/ADR-001-go-y-postgresql.md](adr/ADR-001-go-y-postgresql.md)).
- Protobuf para los mensajes de métricas, si el volumen lo justifica (ver [adr/ADR-003-contratos-json-versionado.md](adr/ADR-003-contratos-json-versionado.md)).
- Historia del consumo por contenedor y por servicio (hoy el panel muestra solo el valor actual);
  serían tablas de métricas propias, particionadas, como las del servidor (decisión del Sprint 2, ver
  [adr/ADR-011-acceso-a-docker-y-journald.md](adr/ADR-011-acceso-a-docker-y-journald.md)).

## Deuda que deja el despliegue del Sprint 0

- **Pipeline de releases.** Hoy el control plane se construye en el propio VPS a partir de un
  `git archive`, y el binario del agent se compila en la máquina de quien despliega. Lo correcto es
  que el CI publique las imágenes en un registro (GHCR) y los binarios del agent firmados, con
  checksum. El diseño ya lo pide para la Fase 4 (actualizaciones firmadas del agent).
- **Revocar agents desde el panel.** Hoy se revoca con una sentencia SQL a mano (ver el runbook).
- **Backups de PostgreSQL** del control plane, con un restore probado. Está en los riesgos del diseño.
- **Mover el control plane a un VPS propio.** Hoy vive en el servidor de un cliente
  ([ADR-007](adr/ADR-007-control-plane-en-servidor-de-cliente.md)). La mudanza está escrita en
  [ADR-014](adr/ADR-014-control-plane-en-vps-propio.md) y en la sección 6 del runbook, y Juan la
  postergó el 2026-09-30. ADR-007 pide hacerla antes de las alertas, de un segundo cliente con
  agent o de las herramientas de escritura (el deploy del Sprint 4).
