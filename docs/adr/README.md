# Decisiones de arquitectura (ADR)

Cada decisión importante se registra acá en un archivo corto, numerado y en español.
Un ADR se escribe **cuando la decisión se toma**, no después, y no se edita una vez aceptado:
si cambia, se escribe uno nuevo que lo reemplaza y se marca el viejo como *reemplazado por ADR-XXX*.

## Formato

```markdown
# ADR-XXX: Título en una línea

- **Estado:** propuesta | aceptada | reemplazada por ADR-YYY
- **Fecha:** AAAA-MM-DD

## Contexto
Qué problema hay que resolver y qué restricciones existen.

## Decisión
Qué se decidió, en presente y sin condicionales.

## Consecuencias
Qué se gana, qué se pierde y qué queda pendiente de revisar.

## Alternativas consideradas
Qué más se evaluó y por qué se descartó.
```

## Índice

| ADR | Título | Estado |
| --- | --- | --- |
| [001](ADR-001-go-y-postgresql.md) | Go y PostgreSQL para agent, control plane y almacenamiento | aceptada |
| [002](ADR-002-agent-conexion-saliente.md) | El agent solo abre conexiones salientes | aceptada |
| [003](ADR-003-contratos-json-versionado.md) | Contratos agent ↔ servidor en JSON versionado | aceptada |
| [004](ADR-004-modulo-go-unico-y-goose.md) | Un único módulo Go y migraciones con goose | aceptada |
| [005](ADR-005-alcance-single-tenant.md) | status-sv es una herramienta interna single-tenant | aceptada |
| [006](ADR-006-autenticacion-de-agents.md) | Autenticación de agents con token de un solo uso y credencial propia | aceptada |
| [007](ADR-007-control-plane-en-servidor-de-cliente.md) | El control plane vive en el servidor de un cliente, detrás de su nginx | reemplazada por 018 |
| [008](ADR-008-tablas-de-metricas.md) | Tablas de métricas, con discos e interfaces aparte, y particiones que maneja el control plane | aceptada |
| [009](ADR-009-agregado-por-minuto-en-la-ingesta.md) | El agregado por minuto se recalcula en la ingesta, no en un job | aceptada |
| [010](ADR-010-usuarios-y-sesiones-del-panel.md) | Usuarios del panel con argon2id y sesiones en la base | aceptada |
| [011](ADR-011-acceso-a-docker-y-journald.md) | Cómo el agent observa Docker y systemd | aceptada |
| [012](ADR-012-proyectos-declarados-en-el-servidor.md) | Los proyectos se declaran en archivos del propio servidor | aceptada |
| [013](ADR-013-segundo-factor-totp.md) | Segundo factor TOTP obligatorio en el panel | aceptada |
| [014](ADR-014-control-plane-en-vps-propio.md) | El control plane vive en un VPS propio, detrás de Caddy | propuesta (postergada) |
| [015](ADR-015-motor-de-deploy-docker.md) | Motor de deploy de proyectos Docker | aceptada |
| [016](ADR-016-pasos-declarados-del-deploy.md) | Pasos declarados del deploy (`deploy.pre` y `deploy.post`) | aceptada |
| [017](ADR-017-deploy-desde-el-panel-y-auditoria.md) | El deploy se dispara desde el panel, con confirmación, y queda en `audit_log` | aceptada |
| [018](ADR-018-control-plane-en-servidor-propio-compartido.md) | El control plane vive en un servidor propio compartido, detrás de su nginx | aceptada, enmendada por 027 |
| [019](ADR-019-deploy-de-proyectos-systemd.md) | Deploy de proyectos systemd, con un ayudante root de operaciones fijas | aceptada |
| [020](ADR-020-que-se-audita-y-como-se-lee.md) | Qué se audita y cómo se lee la auditoría | aceptada |
| [021](ADR-021-primer-deploy-y-reparto-del-deploy-desde-cero.md) | El deploy desde cero se parte: primer deploy ahora, lo que necesita root después | aceptada |
| [022](ADR-022-alta-de-proyectos-propuesta-y-aceptacion.md) | El alta de un proyecto: el control plane propone y una persona acepta en el servidor | aceptada |
| [023](ADR-023-plantillas-sitio-y-certificado.md) | Las plantillas viven en el repo, y el sitio y el certificado los hace el comando de aceptación | aceptada |
| [024](ADR-024-mcp-remoto.md) | El MCP remoto: SDK oficial, sin estado, en `/mcp`, y lo que devuelve | aceptada |
| [025](ADR-025-tokens-del-mcp-y-scopes.md) | Tokens del MCP: se crean en el panel con TOTP, vencen y llevan scopes por entorno y proyecto | aceptada |
| [026](ADR-026-escrituras-por-mcp-y-aprobaciones.md) | Las escrituras por MCP piden aprobación en el panel, con TOTP, sobre un commit fijo | aceptada |
| [027](ADR-027-control-plane-sigue-en-srv936263-con-escrituras.md) | El control plane sigue en `srv936263` también con escrituras por MCP | aceptada |
| [028](ADR-028-backup-cifrado-del-control-plane-en-r2.md) | Backup cifrado del control plane con restic en Cloudflare R2 | aceptada |
