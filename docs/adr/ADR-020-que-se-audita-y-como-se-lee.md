# ADR-020: Qué se audita y cómo se lee la auditoría

- **Estado:** aceptada
- **Fecha:** 2026-10-02

## Contexto

`audit_log` existe desde el Sprint 4 ([ADR-017](ADR-017-deploy-desde-el-panel-y-auditoria.md)):
solo de inserción, con los pedidos de deploy y de rollback, su resultado y los pedidos de logs.
Se lee con SQL, entrando al servidor.

El principio 5 del diseño dice que toda acción deja rastro. Hoy no lo dejan las que tocan el
acceso: entrar al panel, dar de alta el segundo factor, enrolar un agent. Y una auditoría que
solo se lee por SSH no la mira nadie. La decisión la tomó Juan el 2026-10-02.

## Decisión

**1. Se suman las acciones de acceso.**

| Acción | Actor | Resultado |
| --- | --- | --- |
| `auth.login` | el usuario | `ok`, `failed`, `blocked` |
| `auth.totp_verify` | el usuario | `ok`, `failed`, `blocked` |
| `auth.totp_enroll` | el usuario | `ok`, `failed` |
| `auth.recovery_code` | el usuario | `ok` |
| `auth.logout` | el usuario | `ok` |
| `agent.enroll` | el servidor enrolado | `ok`, `rejected`, `blocked` |
| `user.create`, `user.password`, `user.totp_reset` | `cli` (sistema) | `ok` |
| `enrollment_token.create` | `cli` (sistema) | `ok` |

Las que ya estaban (`deploy.request`, `rollback.request`, `deploy.finish`, `logs.request`) no
cambian.

**2. Un login fallido no guarda lo que se escribió.**

Si el nombre no corresponde a un usuario, el actor queda como `desconocido`: en el campo del
usuario a veces se escribe, por error, la contraseña. Se guarda la IP.

**3. Un bloqueo por límite de intentos deja un renglón por IP cada tanto, no uno por pedido.**

Los intentos ya están limitados por IP. Sin esto, quien golpea el login escribiría un renglón por
pedido rechazado en una tabla que no se borra.

**4. Se lee por `GET /v1/audit`, con la sesión completa del panel.**

- Del más nuevo al más viejo, de a 50 (hasta 100), con un cursor: el instante y el ID del último
  renglón que se vio. Nada de páginas numeradas, que se corren cuando entran renglones nuevos.
- Filtros: tipo de actor, acción, tipo e ID del objetivo, resultado y un rango de fechas. Todos
  contra listas cerradas o formatos validados.
- Leer la auditoría no se audita: llenaría la tabla de ruido.

**5. En el panel es una página propia, "Auditoría".**

Con los filtros, el detalle de cada renglón y un enlace desde la historia de deploys de un
proyecto, ya filtrado por ese proyecto. No hay un enlace por deploy: el objetivo de los renglones
de un deploy es su proyecto. Sin exportar.

**6. La tabla no cambia: sin particiones y sin retención.**

Son decenas de renglones por día. Se revisa si pasa del millón.

## Consecuencias

**Positivas**

- Lo que cambia el acceso al panel queda registrado igual que lo que cambia un servidor.
- La auditoría se mira desde el panel, donde ya se está cuando algo llama la atención.

**Negativas**

- Los subcomandos de CLI se auditan desde el propio proceso: quien tiene SSH y la base puede
  saltearlo. Es rastro para el uso normal, no una defensa contra root.
- Los logins fallidos suman renglones; el límite por IP los acota.
- El `detail` es JSON libre para quien escribe el renglón. La regla sigue siendo la de ADR-017:
  códigos, commits e identificadores, nunca secretos ni texto que venga de afuera sin sanear.

## Alternativas consideradas

- **Guardar el nombre escrito en un login fallido:** ayuda a ver ataques por diccionario, pero
  termina guardando contraseñas escritas en el campo equivocado. Descartado.
- **Paginar con número de página:** más simple en el panel, pero con renglones entrando arriba se
  repiten o se saltean. Descartado.
- **Particionar `audit_log` por mes:** no hace falta con este volumen, y complica la regla de
  solo inserción (soltar una partición es borrar). Descartado.
- **Auditar las lecturas** (quién miró qué): no es lo que pide el principio 5, que habla de
  acciones. Descartado.
