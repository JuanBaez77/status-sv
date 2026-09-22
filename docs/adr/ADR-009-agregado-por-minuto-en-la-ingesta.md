# ADR-009: El agregado por minuto se recalcula en la ingesta, no en un job

- **Estado:** aceptada
- **Fecha:** 2026-09-22

## Contexto

El diseño decía que "un job interno agrega `metrics_raw` a `metrics_1m`". El problema son las
muestras que llegan tarde, que no son raras:

- El agent guarda hasta 10 minutos de muestras mientras está desconectado y las reenvía al
  reconectar, así que un minuto puede completarse bastante después de terminar.
- La ingesta acepta muestras de hasta 48 horas atrás.
- Un lote puede llegar dos veces si se pierde el acuse.

Un job cada minuto con una ventana fija (por ejemplo, los últimos 15 minutos) dejaría sin agregar
lo que llegue más tarde que la ventana. Un job que busque diferencias entre el crudo y el agregado
es correcto, pero recorre 48 horas de crudo cada minuto.

## Decisión

**La ingesta recalcula, en la misma transacción del lote, los minutos que ese lote tocó**, por
decisión de Juan.

- Cada lote trae entre 10 s y unos minutos de muestras, así que toca uno o dos minutos. Para esos
  minutos se recalcula el agregado **desde todo el crudo del minuto** y se pisa lo que hubiera
  (`INSERT … ON CONFLICT DO UPDATE`). Como no se suma sobre lo ya agregado, da igual si una muestra
  llega tarde o repetida.
- Si el lote no trajo ninguna fila nueva (un reenvío), no se recalcula nada.
- **Red:** los contadores son acumulados, así que la tasa sale de cada par de muestras seguidas de
  una interfaz y cuenta para el minuto de la segunda. El promedio del minuto es el total de bytes
  sobre el total de segundos, y el pico, la tasa más alta de un par. Un par con un contador que
  baja (el servidor se reinició) o separado por más de 10 minutos (el agent estuvo caído) no es
  una tasa y se descarta. Los 10 minutos son el doble del intervalo de muestreo más largo que
  acepta el contrato.
- Como una muestra que llega tarde parte en dos el par que la rodeaba, la red se recalcula también
  en los 10 minutos siguientes al lote.
- Los minutos se cortan en UTC, igual que las particiones (ADR-008).

## Consecuencias

**Positivas**

- El agregado nunca queda atrás del crudo: si la muestra está guardada, su minuto está al día.
  Incluido el minuto en curso, que el panel puede mostrar sin esperar a que termine.
- No hay job, ni marca de hasta dónde se agregó, ni nada que se pueda trabar sin que se note.
- Es barato: con 10 s de muestreo, recalcular un minuto son seis filas por tabla.

**Negativas**

- La transacción de cada lote hace tres consultas más. Se mide en la prueba de carga del Sprint 1
  (10 agents).
- Si falla la agregación (por ejemplo, porque falta la partición del mes en `metrics_1m`), falla el
  lote entero y el agent lo reintenta. Es el mismo modo de falla que ya tiene el crudo sin
  partición, y el mantenimiento crea las del agregado con 32 días de anticipación.
- El agregado por hora (Sprint 3) va a tener que decidir lo mismo. Lo natural es recalcular la
  hora a partir de `metrics_1m` en el mismo lugar.

## Alternativas consideradas

- **Job cada minuto con una ventana fija:** lo que decía el diseño. Descartado porque pierde lo
  que llega más tarde que la ventana, que es justo el caso de un agent que estuvo desconectado.
- **Job que compara cuántas muestras hay por minuto en el crudo y en el agregado:** correcto, pero
  recorre todo el crudo cada minuto para encontrar unas pocas diferencias.
- **Vistas materializadas:** PostgreSQL no las refresca de forma incremental; refrescarlas es
  recalcular todo. Descartado.
