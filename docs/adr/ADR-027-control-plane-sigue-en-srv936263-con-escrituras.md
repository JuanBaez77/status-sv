# ADR-027: El control plane sigue en `srv936263` también con escrituras por MCP

- **Estado:** aceptada
- **Fecha:** 2026-11-05
- **Enmienda a:** [ADR-018](ADR-018-control-plane-en-servidor-propio-compartido.md), en su sección
  "Cuándo revisarla"

## Contexto

[ADR-018](ADR-018-control-plane-en-servidor-propio-compartido.md) dejó el control plane en
`srv936263`, un servidor propio que comparte con otros productos de Juan (ctrlgym, ExtendHUB y unos
veinte sitios de nginx). Pidió pasarlo al VPS dedicado de
[ADR-014](ADR-014-control-plane-en-vps-propio.md) antes de que hubiera tokens del MCP con permiso de
escritura. El 2026-10-09 se decidió hacer la mudanza al cierre del Sprint 7. No se hizo: no hay un
VPS dedicado.

El Sprint 8 trae esas escrituras ([ADR-026](ADR-026-escrituras-por-mcp-y-aprobaciones.md)). El
2026-11-05 Juan decidió que el control plane se queda en `srv936263`.

## Decisión

**El control plane sigue en `srv936263`, también con las escrituras por MCP.** El VPS dedicado deja
de ser un requisito del Sprint 8 y queda como destino deseable, sin fecha.

Se acepta porque lo que gana un atacante que se adueña del control plane está acotado por cosas
que no dependen del servidor donde vive:

- **El agent no ejecuta texto libre** (regla 1). Ningún comando permite correr algo que no sea una
  de las operaciones de la lista cerrada, sobre un proyecto declarado en el servidor por root.
- **El código que despliega sale del repo declarado, por la rama declarada.** El agent trae con su
  deploy key de solo lectura y rechaza un commit que no esté en esa rama. Para correr código
  propio, un atacante necesita además poder pushear al repo, que es otra credencial.
- **El deploy se prende servidor por servidor**, en el servidor, por root
  (`install-agent.sh --enable-deploy`). Nunca en `srv936263`.
- **Las aprobaciones piden TOTP** ([ADR-026](ADR-026-escrituras-por-mcp-y-aprobaciones.md)). Un token
  del MCP robado puede pedir, no ejecutar.

Lo que sí puede un control plane tomado es lo mismo que ya podía desde el Sprint 4 con el panel:
desplegar o volver atrás a commits que ya están en la rama, reiniciar y leer. Es un riesgo de
disponibilidad de los proyectos, no de ejecución de código arbitrario en los servidores.

**Lo que se mantiene, y ahora es condición para seguir acá:**

- **Ningún contenedor de `srv936263` monta el socket de Docker ni corre `privileged`.** Es lo que
  mostró el relevamiento del 2026-10-01. Se revisa antes de cada actualización del control plane,
  con un comando de solo lectura del runbook.
- **Nunca un agent con el deploy prendido en `srv936263`.**
- **Un respaldo de la base del control plane antes de cada actualización**, como hasta ahora.

## Cuándo revisarla

Pasar al VPS dedicado, que sigue siendo el destino, **antes** de cualquiera de estas cosas:

- Que en `srv936263` corra un producto de un tercero o de un cliente.
- Que el relevamiento encuentre ahí un contenedor con el socket de Docker o `privileged`.
- Que haya un incidente de seguridad en ese servidor.
- Que haga falta desplegar con status-sv algún proyecto de `srv936263`.
- Que existan las alertas de la Fase 4, que tienen que poder avisar si ese servidor se cae.

## Alternativas consideradas

- **Mudar al VPS dedicado antes del Sprint 8**, como pedía ADR-018. Es lo más limpio, pero cuesta
  un VPS más y su mantenimiento, y Juan decidió no pagarlo por ahora.
- **Construir el Sprint 8 y no desplegarlo** hasta tener el VPS. Deja el MVP terminado en el repo
  pero no en uso, que es justo lo que el sprint quiere probar con el cotizador.

## Consecuencias

**Positivas**

- El Sprint 8 se despliega donde ya corre el control plane, sin una mudanza en el medio.
- No hay un VPS más que pagar ni mantener.

**Negativas, aceptadas a sabiendas**

- Sigue la primera consecuencia negativa de ADR-018, y ahora pesa más. Un agujero en cualquier
  producto de `srv936263` que dé root en el host llega al control plane, y desde ahí a pedir
  deploys, rollbacks y reinicios en los servidores con el deploy prendido, salteando las
  aprobaciones. Está acotado por los puntos de arriba, no eliminado.
- Un monitor sigue sin poder avisar de la caída del servidor donde vive.
- `no-new-privileges` sigue apagado para el control plane (el Docker de snap de ADR-018).
