# Contratos agent ↔ control plane

Este paquete es la **única fuente de verdad** de los mensajes que viajan por el WebSocket.
Lo importan los dos extremos, así que el contrato se escribe una sola vez.

El formato es JSON con un envelope versionado; el porqué está en
[ADR-003](../docs/adr/ADR-003-contratos-json-versionado.md).

## Envelope

```json
{
  "v": 1,
  "type": "heartbeat",
  "id": "01924f9c-0000-7000-8000-000000000001",
  "ts": "2026-09-20T12:00:00Z",
  "data": { }
}
```

| Campo | Para qué |
| --- | --- |
| `v` | Versión del protocolo. El receptor rechaza con error explícito lo que no conoce. |
| `type` | Tipo de mensaje, de una lista cerrada. Un tipo desconocido se rechaza, no se ignora. |
| `id` | UUID v7 del mensaje. Sirve para el acuse y para que los comandos sean idempotentes. |
| `ts` | Momento en que se armó el mensaje, en UTC. |
| `data` | El mensaje en sí, según `type`. |

## Mensajes

| Tipo | Dirección | Cuándo | Contenido |
| --- | --- | --- | --- |
| `enroll` | Agent → servidor | Una vez, al instalarse | Token de un solo uso, hostname, versión, sistema y arquitectura |
| `enroll_ack` | Servidor → agent | Respuesta al anterior | ID y nombre del servidor, **credencial propia** y configuración inicial |
| `heartbeat` | Agent → servidor | Cada 15 s | Versión, tiempo encendido y consumo del propio agent |
| `heartbeat_ack` | Servidor → agent | Respuesta al anterior | Hora del servidor y, solo si cambió, la configuración nueva |
| `metrics` | Agent → servidor | Cada 10 s, en lotes | Muestras de CPU, memoria, carga, discos e interfaces |
| `metrics_ack` | Servidor → agent | Respuesta al anterior | Cuántas muestras se guardaron y la marca de tiempo de la última |
| `error` | Servidor → agent | Ante un rechazo | Código de una lista cerrada, texto para el log y el ID del mensaje rechazado |

La credencial de `enroll_ack` viaja **una sola vez**: el control plane guarda solo su hash y el
agent la escribe con permisos `0600`. Si se pierde, hay que enrolar de nuevo.

Los códigos de error son `invalid_token`, `expired_token`, `unauthorized`,
`unsupported_version`, `invalid_message`, `rate_limited` e `internal`. El agent decide qué hacer
mirando el código, nunca el texto: `Retryable()` dice cuáles tiene sentido reintentar.

## Reglas de versionado

- Agregar un campo opcional **no** cambia `v`. Quitar un campo o cambiar su significado, **sí**.
- Los campos desconocidos se ignoran al leer, para que un agent viejo tolere un control plane nuevo.
- Una versión que el receptor no conoce se rechaza con `unsupported_version`; no se adivina.
- Todo mensaje se valida al entrar: los límites de tamaño están para acotar lo que llega de la red.

## Cambiar el contrato

Los archivos de `testdata/` fijan el formato exacto que viaja por el cable. Si un cambio los
mueve, el test falla y el diff muestra qué se modificó, que es justo lo que hay que revisar con
cuidado porque del otro lado puede haber un agent viejo.

```bash
go test ./proto/ -run TestGolden -update
```

Los valores de esos archivos son de ejemplo: no hay ningún token ni credencial real.
