# ADR-008: Tablas de métricas, con discos e interfaces aparte, y particiones que maneja el control plane

- **Estado:** aceptada
- **Fecha:** 2026-09-21

## Contexto

El diseño define `metrics_raw` (partición diaria, 48 h) y `metrics_1m` (partición mensual, 30 días)
con "CPU, RAM, disco, red y carga", pero no dice cómo se guardan los discos y las interfaces, que
son listas de largo variable: un servidor tiene uno o diez discos, y dos o cinco interfaces.

Además, ADR-001 eligió el particionado nativo de PostgreSQL en vez de TimescaleDB, y dejó anotado
que crear las particiones futuras y soltar las vencidas es un trabajo propio que hay que escribir.
Falta decidir quién lo hace y cómo, sin armar SQL con texto (regla del proyecto) aunque el DDL no
acepte parámetros.

## Decisión

**1. Discos e interfaces van en tablas aparte**, por decisión de Juan: una fila por disco o por
interfaz y por muestra, con la misma marca de tiempo que la muestra.

| Nivel | Muestra | Discos | Interfaces | Partición | Retención |
| --- | --- | --- | --- | --- | --- |
| Crudo | `metrics_raw` | `metrics_raw_disks` | `metrics_raw_networks` | Diaria | 48 h |
| Por minuto | `metrics_1m` | `metrics_1m_disks` | `metrics_1m_networks` | Mensual | 30 días |

La clave primaria de cada tabla es `(server_id, ts)`, o `(server_id, ts, mountpoint)` e
`(server_id, ts, interface)`. Sirve para dos cosas: es el índice que usa el panel para pedir un
servidor en un rango, y es lo que hace idempotente la ingesta, porque el agent puede mandar un lote
dos veces.

Del agregado por minuto se guarda el promedio y el máximo de CPU y de memoria (un pico no puede
desaparecer en el promedio), el peor momento de cada disco (lo más usado y lo menos disponible) y
las tasas de red ya calculadas, en bytes por segundo, promedio y pico.

**2. Las particiones las maneja el control plane**, con dos funciones de la migración:

- `metrics_ensure_partitions(tabla, desde, hasta)` crea las que falten.
- `metrics_drop_partitions(tabla, antes_de)` borra las que terminan antes del corte.

El control plane las llama al arrancar y después cada hora, para las seis tablas, con 48 h de
anticipación en el crudo y 32 días en el agregado. Si falla, reintenta al minuto.

- **Nombres fijos:** `<tabla>_p<AAAAMMDD>` o `<tabla>_p<AAAAMM>`. Una partición con otro nombre
  (creada a mano) no se borra nunca sola.
- **UTC siempre:** las funciones corren con `TimeZone = 'UTC'`, así los cortes caen a medianoche
  UTC sin importar la zona de la sesión ni los cambios de horario.
- **SQL dinámico escapado:** el DDL no acepta parámetros, así que las funciones arman la sentencia
  con `format('%I', ...)` y `format('%L', ...)`, y solo aceptan las seis tablas de una lista
  cerrada. Desde Go se llaman como cualquier consulta de sqlc, con parámetros.
- **Sin partición por defecto:** una muestra fuera de rango se rechaza. La ingesta descarta lo que
  sea más viejo que la retención o esté demasiado en el futuro, y la anticipación de 48 h cubre un
  reloj corrido.
- **Candado consultivo:** dos control planes a la vez (durante un despliegue) no pueden crear la
  misma partición.

## Consecuencias

**Positivas**

- Borrar lo vencido es soltar una partición: instantáneo y sin inflar la tabla.
- Cada disco y cada interfaz es una columna tipada, con sus restricciones (`CHECK`) y su índice.
  Consultar "cómo evolucionó `/` en la última semana" es un `WHERE` simple, sin abrir JSON.
- Un disco nuevo o una interfaz que desaparece no cambian el esquema.
- Las funciones se prueban contra PostgreSQL real en CI, incluida la zona horaria.

**Negativas**

- Cada muestra son varias filas: con tres discos y dos interfaces, seis filas cada 10 s por
  servidor. Para decenas de servidores sigue siendo poco (unas 50.000 filas por día y servidor en
  el crudo, que viven tres días).
- La ingesta escribe en tres tablas por lote, y tiene que hacerlo en una transacción para que una
  muestra no quede a medias.
- Como se borra de a particiones enteras, lo guardado va de la retención a la retención más una
  partición: el crudo, entre 48 y 72 horas; el agregado, entre 30 días y dos meses.
- Si el mantenimiento deja de correr, en dos días las muestras nuevas empiezan a rechazarse. Se ve
  en el registro con cada reintento; cuando haya alertas (Fase 4), va a ser una de ellas.

## Alternativas consideradas

- **Discos e interfaces como JSONB en la fila de la muestra:** una sola fila por muestra, pero sin
  tipos ni restricciones, y cada consulta del panel tiene que abrir el JSON. Descartado por Juan.
- **Una columna por disco o por interfaz:** el esquema dependería de cada servidor. Descartado.
- **pg_partman:** resuelve el mantenimiento, pero es una extensión que no viene en la imagen
  oficial de PostgreSQL y que habría que instalar y actualizar. Descartado.
- **Partición por defecto:** evitaría rechazar muestras fuera de rango, pero cada partición nueva
  obliga a revisar la de defecto, y una muestra con el reloj corrido se guardaría en un lugar que
  nadie consulta ni borra. Descartado.
- **Particiones del agregado por semana:** retención más ajustada (30 a 37 días), con cuatro veces
  más particiones. Se mantiene la mensual del plan; se revisa si el volumen lo pide.
- **Mantenimiento desde un cron del sistema:** otro componente que desplegar y vigilar, fuera del
  binario. Descartado.
