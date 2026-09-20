# ADR-003: Contratos agent ↔ servidor en JSON versionado

- **Estado:** aceptada
- **Fecha:** 2026-09-20

## Contexto

Agent y control plane intercambian mensajes por WebSocket: `enroll`, `heartbeat`, `metrics`,
`runtime_state`, `deploy_event`, `command` y `config_update`. Los agents se actualizan de a uno y
en servidores ajenos, así que en cualquier momento va a haber versiones distintas hablando con el
mismo control plane: el formato tiene que poder evolucionar sin romper a los agents viejos.

El diseño dejó abierta la elección entre Protobuf (más compacto y con contrato estricto) y JSON
(más simple de depurar). Los dos extremos están escritos en Go, así que el argumento de
interoperabilidad entre lenguajes no aplica.

## Decisión

Los mensajes se definen como **structs de Go en el paquete `proto/`**, serializados en **JSON**,
dentro de un **envelope con versión**:

```json
{ "v": 1, "type": "heartbeat", "id": "<uuid v7>", "ts": "<RFC3339 en UTC>", "data": {} }
```

Reglas de versionado:

- `v` es la versión del protocolo. El receptor rechaza con un error explícito toda versión que no
  conoce, en vez de adivinar.
- Agregar un campo opcional **no** cambia `v`. Quitar un campo o cambiar su significado, sí.
- `id` identifica el mensaje y permite acuse e idempotencia de los comandos.
- Los campos desconocidos se ignoran al deserializar, para que un agent viejo tolere un servidor nuevo.
- Todo cambio en `proto/` viene con tests de round-trip y golden files en el mismo PR.

## Consecuencias

**Positivas**

- Un mensaje se lee a ojo en un log o en el inspector del navegador, que durante la curva de Go
  vale más que unos bytes de ahorro.
- No hay que sumar `protoc`, `buf` ni un paso de generación al entorno de desarrollo ni al CI.
- Los structs de Go son la única fuente de verdad, compartida por los dos lados del canal.

**Negativas**

- Los lotes de métricas ocupan más que en binario. Es aceptable en el MVP: con muestras cada 10 s y
  decenas de servidores, el tráfico es de pocos KB por minuto y por agent.
- JSON no da validación de esquema por sí solo: la validación se escribe a mano en cada mensaje y
  hay que cubrirla con tests.

**Pendiente de revisar**

- Si el tráfico de métricas se vuelve el cuello de botella, se migra **solo el mensaje `metrics`**
  a Protobuf, con un ADR nuevo y subiendo `v`. El envelope ya está preparado para esa transición.
  Criterio concreto: más de 1 MB por minuto y por agent, o CPU del control plane dominada por la
  serialización en la prueba de carga del Sprint 1.
- Antes de cambiar de formato se evalúa compresión por mensaje del WebSocket
  (`permessage-deflate`), que es mucho más barata de adoptar. Se mide en el Sprint 1.

## Alternativas consideradas

- **Protobuf desde el inicio:** más compacto y con contrato estricto, pero agrega generación de
  código al CI y a la curva de aprendizaje, y complica depurar el canal justo cuando más falta
  hace verlo. Se descarta ahora, no para siempre.
- **JSON Schema como fuente de verdad, con structs generados:** una herramienta más y un paso de
  generación, para un beneficio chico teniendo Go en los dos extremos. Descartado.
- **MessagePack o CBOR:** compacto y sin generación de código, pero pierde la legibilidad que es la
  razón principal de elegir JSON. Descartado.
