# ADR-017: El deploy se dispara desde el panel, con confirmación, y queda en `audit_log`

- **Estado:** aceptada
- **Fecha:** 2026-09-30

## Contexto

En el Sprint 4 el deploy se dispara desde el panel; el MCP y las aprobaciones humanas llegan en la
Fase 3. Hasta entonces, quien despliega es una persona con sesión completa (contraseña y TOTP,
[ADR-013](ADR-013-segundo-factor-totp.md)).

El diseño pone `audit_log` en el Sprint 5, pero el deploy es la primera acción que cambia algo en
un servidor, y el principio 5 dice que toda acción deja rastro.

## Decisión

**1. Desplegar pide una confirmación que no se puede dar sin querer.**

- En el panel hay que escribir el nombre del proyecto.
- En `production`, además, un código TOTP nuevo en el mismo pedido (no un código de recuperación).
  El control plane lo verifica igual que en el login y no acepta dos veces el mismo código.
- Una sesión robada (la cookie sola) no alcanza para desplegar en producción.

**2. `audit_log` entra en el Sprint 4, en su versión mínima.**

- Tabla de solo inserción: momento, actor, tipo de actor (`user`, `llm`, `agent`, `system`),
  acción, tipo y ID del objetivo, resultado, IP y un detalle corto en JSON.
- Un trigger rechaza `UPDATE`, `DELETE` y `TRUNCATE`, y además se le revocan esos permisos al rol
  de la aplicación. Con el trigger alcanza aunque la aplicación sea dueña de la tabla.
- Se registran: pedir un deploy o un rollback (aceptado o rechazado, y por qué) y su resultado
  final. Los pedidos de logs, que hoy van solo al log del proceso, pasan también a `audit_log`.
- La vista de auditoría en el panel queda para el Sprint 5.

**3. Un deploy activo por proyecto, garantizado por la base.**

Un índice único parcial sobre `deploys (project_id)` para los estados `pending` y `running`: dos
pedidos simultáneos no pueden ganar los dos, ni aunque lleguen a la vez.

## Consecuencias

**Positivas**

- Desplegar en producción exige tener el teléfono en la mano, no solo la sesión abierta.
- El primer deploy real ya queda auditado.

**Negativas**

- Un paso más en cada deploy de producción. Es lo que se busca.
- Hasta el Sprint 5, la auditoría se lee con SQL.

## Alternativas consideradas

- **Solo un diálogo de confirmación:** no protege de una sesión robada. Descartado para producción.
- **Esperar al Sprint 5 para `audit_log`:** dejaría los primeros deploys reales sin rastro en la
  base. Descartado.
- **Bloquear deploys simultáneos solo en memoria:** se pierde con un reinicio del control plane y
  no cubre dos réplicas. Descartado.
