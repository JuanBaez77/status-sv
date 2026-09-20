# ADR-005: status-sv es una herramienta interna single-tenant

- **Estado:** aceptada
- **Fecha:** 2026-09-20

## Contexto

El documento de diseño dejaba abierta una pregunta que afecta al modelo de datos entero: si
status-sv es una herramienta interna para administrar los propios VPS, o un producto que otras
personas van a contratar. La diferencia no es cosmética: si hay terceros, cada tabla necesita una
columna de organización, cada consulta un filtro, cada endpoint un control de pertenencia, y hacen
falta invitaciones, roles y facturación.

## Decisión

status-sv es **una herramienta interna single-tenant**: todos los servidores, proyectos y usuarios
pertenecen a la misma organización implícita, que no se modela.

En concreto:

- **No** hay tabla de organizaciones ni columna `organization_id` en ninguna tabla.
- `users` son cuentas del panel con contraseña argon2id y TOTP, sin roles finos: la distinción que
  importa es si la cuenta puede aprobar operaciones de escritura en producción.
- El aislamiento que sí existe es el de **scopes por proyecto y entorno en los tokens del MCP**,
  que protege contra un token filtrado, no contra otro cliente.

## Consecuencias

**Positivas**

- El modelo de datos se mantiene en las doce tablas del diseño, sin columnas ni joins de más.
- Cada endpoint y cada consulta son más simples, y hay menos superficie para un error de
  aislamiento, que es la clase de bug más cara en un producto multi-tenant.
- El MVP llega antes, que es el riesgo número uno del proyecto.

**Negativas**

- Si algún día status-sv se ofrece a terceros, la migración es real: agregar la columna a todas las
  tablas, rellenarla, hacerla obligatoria, y revisar cada consulta y cada endpoint.

**Cómo se mitiga esa migración futura**

- Todo el acceso a datos pasa por `server/internal/store`, generado con sqlc: el día que haya que
  agregar el filtro, hay un solo lugar donde tocar y el compilador ayuda.
- Los IDs son UUID v7, así que no hay que reasignar claves si dos instancias se fusionan.
- Ningún endpoint asume el único servidor ni el único usuario: todo se consulta por ID.

## Alternativas consideradas

- **Multi-tenant desde el inicio:** más caro en cada tabla y cada endpoint, para una necesidad que
  hoy no existe y que puede no existir nunca. Descartado.
- **Single-tenant pero con `organization_id` reservado en el esquema:** el costo de la columna es
  bajo, pero una columna que siempre tiene el mismo valor no se prueba nunca y da una falsa
  sensación de estar preparado; el trabajo duro de multi-tenant está en las consultas y los
  permisos, no en la columna. Descartado.
