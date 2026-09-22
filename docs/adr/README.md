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
| [007](ADR-007-control-plane-en-servidor-de-cliente.md) | El control plane vive en el servidor de un cliente, detrás de su nginx | aceptada |
| [008](ADR-008-tablas-de-metricas.md) | Tablas de métricas, con discos e interfaces aparte, y particiones que maneja el control plane | aceptada |
