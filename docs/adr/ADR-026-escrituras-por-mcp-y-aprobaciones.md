# ADR-026: Las escrituras por MCP piden aprobación en el panel, con TOTP, sobre un commit fijo

- **Estado:** aceptada
- **Fecha:** 2026-10-12

## Contexto

El Sprint 7 dejó el MCP de lectura en producción ([ADR-024](ADR-024-mcp-remoto.md)), con tokens
por entorno y proyecto ([ADR-025](ADR-025-tokens-del-mcp-y-scopes.md)). El Sprint 8 suma las
escrituras: `deploy`, `rollback` y `restart`, más `register_project` y `bootstrap_project`. Con eso,
Claude Code puede hacer el flujo completo: el PR, el merge, el deploy y la verificación. Es el
criterio de salida de la Fase 3 y del MVP.

La regla 4 del proyecto fija el piso: las herramientas de escritura sobre producción devuelven
`pending_approval` y se ejecutan recién cuando una persona aprueba, y el bootstrap siempre exige
aprobación. El diseño dejaba abiertas dos preguntas:

- si la aprobación se hace solo desde el panel o también con un enlace firmado;
- si staging también pide aprobación.

Falta decidir además:

- qué se aprueba exactamente;
- cuánto dura una aprobación;
- qué pasa con un pedido cuyo token se revoca;
- cómo es `restart`, que el agent todavía no tiene.

Un LLM conectado al MCP lee texto que no controla: logs, la salida de los pasos de un deploy, PR e
issues. Ese texto puede traer instrucciones escritas para que pida un deploy. La aprobación es la
barrera, así que tiene que mostrar exactamente lo que se va a ejecutar, y nada que el LLM escriba
puede pasar por la decisión de la persona.

La decisión la tomó Juan el 2026-10-12, aceptando las recomendaciones del plan del Sprint 8.

## Decisión

**1. Qué escribe cada herramienta y qué pide.**

| Herramienta | Parámetros | Scope | Aprobación |
| --- | --- | --- | --- |
| `deploy` | proyecto y commit: un SHA completo de 40 caracteres, obligatorio | `deploy:<entorno>` | siempre |
| `rollback` | proyecto; vuelve al último estado bueno, sin commit | `rollback:<entorno>` | siempre |
| `restart` | proyecto y, opcional, una unidad suya | `restart:<entorno>` | siempre |
| `bootstrap_project` | proyecto aceptado que todavía no corre; la punta de su rama | `deploy:<entorno>` | siempre |
| `register_project` | la declaración propuesta, como en el panel | `deploy:<entorno>` | no: la aceptación en el servidor es la aprobación ([ADR-022](ADR-022-alta-de-proyectos-propuesta-y-aceptacion.md)) |
| `get_approval` | el ID de un pedido | el de su herramienta | no: solo lee |

- Las cuatro primeras **no le mandan nada al agent**: guardan un pedido pendiente y devuelven
  `pending_approval`, con su ID y su vencimiento. El LLM sigue el pedido con `get_approval`, que
  dice en qué quedó y, si salió, el ID del deploy, que se mira con `get_deploy`. No hay avisos por
  SSE: el MCP sigue sin estado ([ADR-024](ADR-024-mcp-remoto.md)).
- Un servidor sigue necesitando el deploy prendido (`install-agent.sh --enable-deploy`) para
  cualquier escritura, también para `restart`. Un servidor donde nadie lo prendió no acepta
  escrituras, aunque las pida un token con todos los scopes.
- Son los tres pares de scopes que preveía ADR-025, con la misma lista cerrada en la base.

**2. Se aprueba solo en el panel, con un código TOTP nuevo.**

- Aprobar pide la sesión completa, el nombre del proyecto escrito a mano y un código TOTP nuevo,
  como un deploy de producción desde el panel ([ADR-017](ADR-017-deploy-desde-el-panel-y-auditoria.md)).
  Rechazar no pide código: frenar tiene que ser fácil.
- **Ningún token aprueba.** No existe una herramienta del MCP que apruebe ni que rechace: un LLM no
  puede aprobar lo que él mismo pidió.
- **Sin enlace firmado.** El panel ya anda en el celular. Un enlace firmado tendría que pedir el
  código igual, y sumaría un camino de entrada más (un enlace reenviado o filtrado).

**3. Qué ve quien aprueba.**

- La operación, el proyecto, el servidor y el entorno.
- Para un `deploy`, el commit completo con un enlace a él en el repo declarado, y el commit que
  corre ahora.
- El token que lo pidió: su nombre, su prefijo y la IP del pedido.
- Cuándo vence.
- El **motivo**, si el LLM lo dio: un texto opcional de hasta 500 caracteres. Va aparte, marcado
  como texto del LLM y no confiable, igual que los logs en el MCP. No se interpreta ni cambia nada
  del pedido.

Lo que se ejecuta sale de los campos tipados del pedido, nunca del motivo.

**4. El commit queda fijo.**

- Un `deploy` por MCP lleva el commit completo. La persona aprueba exactamente ese commit, y el
  agent despliega ese: no "la punta de `main`", que podría cambiar entre el pedido y la aprobación.
- El agent ya rechaza un commit que no está en la rama declarada (`merge-base --is-ancestor`): un
  LLM no puede colar un commit de otra rama ni de un fork.
- `rollback` y `bootstrap_project` no llevan commit, como desde el panel. La aprobación muestra a
  qué vuelve o qué rama trae.

**5. Cuánto vive un pedido.**

- Vence a la hora. Hay **uno pendiente por proyecto**: otro pedido sobre el mismo proyecto se
  rechaza y nombra el que está esperando.
- Se cancela solo si el token que lo pidió se revoca o vence, o si el proyecto deja de estar
  declarado.
- Al aprobar se vuelve a verificar todo antes de mandarle algo al agent:
  - que el token siga vivo y con el scope;
  - que el proyecto exista;
  - que el agent esté conectado;
  - que no haya otro deploy en curso.

  Si algo falla, la aprobación no ejecuta nada y queda anotado el motivo.
- Los estados son pendiente, aprobado, rechazado, vencido y cancelado. Un pedido decidido no
  cambia más.

**6. Aprobar ejecuta por el mismo camino que el panel.**

- Una aprobación dispara el mismo deploy, rollback o primer deploy que el botón del panel. El agent
  hace lo de siempre: trae el código, compila, levanta, verifica el healthcheck y vuelve atrás solo
  si falla.
- El deploy guarda quién lo pidió (el token, como `llm`) y quién lo aprobó (la persona).

**7. `restart` es una operación nueva del agent, tipada.**

- Reinicia las unidades de un proyecto declarado, o una sola si se nombra:
  - en Docker, los servicios del proyecto de Compose;
  - en systemd, su unidad, por el ayudante root, como un paso más de su lista fija
    ([ADR-019](ADR-019-deploy-de-proyectos-systemd.md)).
- Recibe identificadores, nunca texto libre. Una unidad que no es del proyecto se rechaza, como en
  `get_logs`.
- No cambia código y no tiene rollback. Después de reiniciar, el agent espera el healthcheck y
  devuelve si pasó.

**8. En el Sprint 8 todo pide aprobación, también staging.**

Hoy no hay proyectos de staging. Cuando haya, se decide en otro ADR si staging puede andar sin
aprobación. El diseño lo prevé, pero hoy sería código que nada usa.

**9. Auditoría y límites.**

- Cada pedido, aprobación, rechazo, vencimiento y cancelación queda en `audit_log`
  (`approval.request` como `llm`, `approval.approve` y `approval.reject` como persona,
  `approval.expire` y `approval.cancel` como sistema), con el ID del pedido. El deploy que sale
  queda además con sus acciones de siempre.
- Las llamadas de escritura tienen su propio cupo por token, más chico que el de lectura: seis
  pedidos por hora.

## Alternativas consideradas

- **Aprobar "la punta de `main`" sin fijar el commit.** Es más cómodo para el LLM, pero se aprueba
  una cosa y se puede desplegar otra: un push entre el pedido y la aprobación cambia lo que corre.
- **Un enlace firmado para aprobar desde el celular.** Descartado (punto 2). El panel ya anda en el
  celular.
- **Aprobar por MCP con otro token**, por ejemplo uno "de aprobación". Descartado: cualquier
  credencial que un LLM pueda usar para aprobar termina siendo la del mismo LLM.
- **Staging sin aprobación desde ya.** Postergado (punto 8): no hay proyectos de staging.
- **`restart` por `docker restart` de cualquier contenedor.** Descartado: sería una operación sobre
  el servidor, no sobre un proyecto declarado (regla 2).

## Consecuencias

**Positivas**

- Un LLM engañado por un log o un PR puede como mucho **pedir**. Lo que se ejecuta es lo que una
  persona vio y aprobó con su segundo factor: ese commit, de esa rama, en ese proyecto.
- El deploy por MCP reusa todo el motor del panel. No hay un segundo camino de escritura que
  mantener.
- La auditoría responde quién pidió, quién aprobó y qué salió.

**Negativas**

- Cada escritura pide a alguien con el teléfono a mano, también un `restart`. Es el precio de la
  regla 4 hasta que haya staging.
- El LLM tiene que conseguir el SHA completo del commit (con `git` o `gh`) antes de pedir un deploy.
- `restart` cambia el agent y el ayudante: hay que actualizar los agents para usarlo.
- Una migración de base que corre al arrancar un proyecto no se deshace con el rollback. Eso no lo
  resuelve la aprobación: queda en el runbook, con un respaldo antes de un deploy que trae
  migraciones.
