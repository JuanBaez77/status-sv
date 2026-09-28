-- Agregado por minuto (ADR-009). Cada consulta recalcula, para un servidor,
-- los minutos que empiezan entre from_ts y to_ts a partir de todo el crudo de
-- esos minutos, y pisa lo que hubiera. Recalcular desde el crudo, en vez de
-- sumar sobre lo ya agregado, es lo que permite que una muestra llegue tarde o
-- dos veces sin que el minuto quede mal.
--
-- Los minutos se cortan en UTC. LEAST protege las restricciones de la tabla
-- (promedio ≤ máximo) de un redondeo de punto flotante: si una fila chocara, el
-- lote entero fallaría y el agent lo reenviaría para siempre.

-- AggregateSamplesByMinute agrega CPU, memoria y carga.
-- name: AggregateSamplesByMinute :execrows
INSERT INTO metrics_1m (
    server_id, bucket, samples, cpu_used_avg, cpu_used_max, cpu_iowait_avg, cpu_iowait_max, cpu_steal_avg,
    cpu_cores, mem_total_bytes, mem_used_avg_bytes, mem_used_max_bytes, swap_total_bytes,
    swap_used_avg_bytes, swap_used_max_bytes, load1_avg, load5_avg, load15_avg
)
SELECT server_id,
       date_trunc('minute', ts, 'UTC'),
       LEAST(count(*), 32767)::smallint,
       LEAST(avg(cpu_used_percent), max(cpu_used_percent))::real,
       max(cpu_used_percent),
       LEAST(avg(cpu_iowait_percent), max(cpu_iowait_percent))::real,
       max(cpu_iowait_percent),
       avg(cpu_steal_percent)::real,
       max(cpu_cores),
       max(mem_total_bytes),
       LEAST(round(avg(mem_used_bytes)), max(mem_used_bytes))::bigint,
       max(mem_used_bytes),
       max(swap_total_bytes),
       LEAST(round(avg(swap_used_bytes)), max(swap_used_bytes))::bigint,
       max(swap_used_bytes),
       avg(load1)::real,
       avg(load5)::real,
       avg(load15)::real
FROM metrics_raw
WHERE server_id = sqlc.arg('server_id')::uuid
  AND ts >= sqlc.arg('from_ts')::timestamptz
  AND ts < sqlc.arg('to_ts')::timestamptz
GROUP BY server_id, date_trunc('minute', ts, 'UTC')
ON CONFLICT (server_id, bucket) DO UPDATE SET
    samples             = EXCLUDED.samples,
    cpu_used_avg        = EXCLUDED.cpu_used_avg,
    cpu_used_max        = EXCLUDED.cpu_used_max,
    cpu_iowait_avg      = EXCLUDED.cpu_iowait_avg,
    cpu_iowait_max      = EXCLUDED.cpu_iowait_max,
    cpu_steal_avg       = EXCLUDED.cpu_steal_avg,
    cpu_cores           = EXCLUDED.cpu_cores,
    mem_total_bytes     = EXCLUDED.mem_total_bytes,
    mem_used_avg_bytes  = EXCLUDED.mem_used_avg_bytes,
    mem_used_max_bytes  = EXCLUDED.mem_used_max_bytes,
    swap_total_bytes    = EXCLUDED.swap_total_bytes,
    swap_used_avg_bytes = EXCLUDED.swap_used_avg_bytes,
    swap_used_max_bytes = EXCLUDED.swap_used_max_bytes,
    load1_avg           = EXCLUDED.load1_avg,
    load5_avg           = EXCLUDED.load5_avg,
    load15_avg          = EXCLUDED.load15_avg;

-- AggregateDisksByMinute guarda el peor momento de cada disco en el minuto: lo
-- más usado y lo menos disponible.
-- name: AggregateDisksByMinute :execrows
INSERT INTO metrics_1m_disks (server_id, bucket, mountpoint, total_bytes, used_max_bytes, available_min_bytes)
SELECT server_id,
       date_trunc('minute', ts, 'UTC'),
       mountpoint,
       max(total_bytes),
       max(used_bytes),
       min(available_bytes)
FROM metrics_raw_disks
WHERE server_id = sqlc.arg('server_id')::uuid
  AND ts >= sqlc.arg('from_ts')::timestamptz
  AND ts < sqlc.arg('to_ts')::timestamptz
GROUP BY server_id, date_trunc('minute', ts, 'UTC'), mountpoint
ON CONFLICT (server_id, bucket, mountpoint) DO UPDATE SET
    total_bytes         = EXCLUDED.total_bytes,
    used_max_bytes      = EXCLUDED.used_max_bytes,
    available_min_bytes = EXCLUDED.available_min_bytes;

-- AggregateNetworksByMinute pasa los contadores acumulados a tasas en bytes por
-- segundo. Cada par de muestras seguidas de una interfaz da una tasa, que
-- cuenta para el minuto de la segunda. El promedio del minuto es el total de
-- bytes sobre el total de segundos, y el pico, la tasa más alta de un par.
--
-- Para el primer par hace falta la muestra anterior al minuto: por eso se lee
-- desde prev_from_ts. Un par separado por más de max_gap_seconds (el agent
-- estuvo caído) o con un contador que bajó (el servidor se reinició) no es una
-- tasa, y se descarta.
-- name: AggregateNetworksByMinute :execrows
WITH pairs AS (
    SELECT interface,
           ts,
           rx_bytes - lag(rx_bytes) OVER w                 AS rx_delta,
           tx_bytes - lag(tx_bytes) OVER w                 AS tx_delta,
           extract(epoch FROM ts - lag(ts) OVER w)::float8 AS seconds
    FROM metrics_raw_networks
    WHERE server_id = sqlc.arg('server_id')::uuid
      AND ts >= sqlc.arg('prev_from_ts')::timestamptz
      AND ts < sqlc.arg('to_ts')::timestamptz
    WINDOW w AS (PARTITION BY interface ORDER BY ts)
), rates AS (
    SELECT date_trunc('minute', ts, 'UTC') AS bucket,
           interface,
           rx_delta::float8 AS rx_delta,
           tx_delta::float8 AS tx_delta,
           seconds
    FROM pairs
    WHERE ts >= sqlc.arg('from_ts')::timestamptz
      AND seconds > 0
      AND seconds <= sqlc.arg('max_gap_seconds')::float8
      AND rx_delta >= 0
      AND tx_delta >= 0
)
INSERT INTO metrics_1m_networks (
    server_id, bucket, interface, rx_avg_per_sec, rx_max_per_sec, tx_avg_per_sec, tx_max_per_sec
)
SELECT sqlc.arg('server_id')::uuid,
       bucket,
       interface,
       LEAST(sum(rx_delta) / sum(seconds), max(rx_delta / seconds)),
       max(rx_delta / seconds),
       LEAST(sum(tx_delta) / sum(seconds), max(tx_delta / seconds)),
       max(tx_delta / seconds)
FROM rates
GROUP BY bucket, interface
ON CONFLICT (server_id, bucket, interface) DO UPDATE SET
    rx_avg_per_sec = EXCLUDED.rx_avg_per_sec,
    rx_max_per_sec = EXCLUDED.rx_max_per_sec,
    tx_avg_per_sec = EXCLUDED.tx_avg_per_sec,
    tx_max_per_sec = EXCLUDED.tx_max_per_sec;

-- Agregado por hora. Igual que el minuto, cada consulta recalcula las horas que
-- empiezan entre from_ts y to_ts, pero desde metrics_1m: el crudo dura 48 horas
-- y el minuto ya está al día cuando se llega acá, en la misma transacción.
--
-- Los promedios de la hora pesan cada minuto por sus muestras, así un minuto
-- con una sola muestra no vale lo mismo que uno completo.

-- AggregateSamplesByHour agrega CPU, memoria y carga, con el mínimo de CPU y
-- de memoria además del promedio y el máximo.
-- name: AggregateSamplesByHour :execrows
INSERT INTO metrics_1h (
    server_id, bucket, samples, cpu_used_min, cpu_used_avg, cpu_used_max, cpu_iowait_avg, cpu_iowait_max,
    cpu_steal_avg, cpu_cores, mem_total_bytes, mem_used_min_bytes, mem_used_avg_bytes, mem_used_max_bytes,
    swap_total_bytes, swap_used_avg_bytes, swap_used_max_bytes, load1_avg, load5_avg, load15_avg
)
SELECT server_id,
       date_trunc('hour', bucket, 'UTC'),
       LEAST(sum(samples), 2147483647)::integer,
       LEAST(min(cpu_used_avg), sum(cpu_used_avg * samples) / sum(samples))::real,
       LEAST(sum(cpu_used_avg * samples) / sum(samples), max(cpu_used_max))::real,
       max(cpu_used_max),
       LEAST(sum(cpu_iowait_avg * samples) / sum(samples), max(cpu_iowait_max))::real,
       max(cpu_iowait_max),
       (sum(cpu_steal_avg * samples) / sum(samples))::real,
       max(cpu_cores),
       max(mem_total_bytes),
       LEAST(min(mem_used_avg_bytes), round(sum(mem_used_avg_bytes::numeric * samples) / sum(samples)))::bigint,
       LEAST(round(sum(mem_used_avg_bytes::numeric * samples) / sum(samples)), max(mem_used_max_bytes))::bigint,
       max(mem_used_max_bytes),
       max(swap_total_bytes),
       LEAST(round(sum(swap_used_avg_bytes::numeric * samples) / sum(samples)), max(swap_used_max_bytes))::bigint,
       max(swap_used_max_bytes),
       (sum(load1_avg * samples) / sum(samples))::real,
       (sum(load5_avg * samples) / sum(samples))::real,
       (sum(load15_avg * samples) / sum(samples))::real
FROM metrics_1m
WHERE server_id = sqlc.arg('server_id')::uuid
  AND bucket >= sqlc.arg('from_ts')::timestamptz
  AND bucket < sqlc.arg('to_ts')::timestamptz
GROUP BY server_id, date_trunc('hour', bucket, 'UTC')
ON CONFLICT (server_id, bucket) DO UPDATE SET
    samples             = EXCLUDED.samples,
    cpu_used_min        = EXCLUDED.cpu_used_min,
    cpu_used_avg        = EXCLUDED.cpu_used_avg,
    cpu_used_max        = EXCLUDED.cpu_used_max,
    cpu_iowait_avg      = EXCLUDED.cpu_iowait_avg,
    cpu_iowait_max      = EXCLUDED.cpu_iowait_max,
    cpu_steal_avg       = EXCLUDED.cpu_steal_avg,
    cpu_cores           = EXCLUDED.cpu_cores,
    mem_total_bytes     = EXCLUDED.mem_total_bytes,
    mem_used_min_bytes  = EXCLUDED.mem_used_min_bytes,
    mem_used_avg_bytes  = EXCLUDED.mem_used_avg_bytes,
    mem_used_max_bytes  = EXCLUDED.mem_used_max_bytes,
    swap_total_bytes    = EXCLUDED.swap_total_bytes,
    swap_used_avg_bytes = EXCLUDED.swap_used_avg_bytes,
    swap_used_max_bytes = EXCLUDED.swap_used_max_bytes,
    load1_avg           = EXCLUDED.load1_avg,
    load5_avg           = EXCLUDED.load5_avg,
    load15_avg          = EXCLUDED.load15_avg;

-- AggregateDisksByHour guarda el peor momento de cada disco en la hora.
-- name: AggregateDisksByHour :execrows
INSERT INTO metrics_1h_disks (server_id, bucket, mountpoint, total_bytes, used_max_bytes, available_min_bytes)
SELECT server_id,
       date_trunc('hour', bucket, 'UTC'),
       mountpoint,
       max(total_bytes),
       max(used_max_bytes),
       min(available_min_bytes)
FROM metrics_1m_disks
WHERE server_id = sqlc.arg('server_id')::uuid
  AND bucket >= sqlc.arg('from_ts')::timestamptz
  AND bucket < sqlc.arg('to_ts')::timestamptz
GROUP BY server_id, date_trunc('hour', bucket, 'UTC'), mountpoint
ON CONFLICT (server_id, bucket, mountpoint) DO UPDATE SET
    total_bytes         = EXCLUDED.total_bytes,
    used_max_bytes      = EXCLUDED.used_max_bytes,
    available_min_bytes = EXCLUDED.available_min_bytes;

-- AggregateNetworksByHour promedia las tasas de los minutos de la hora y se
-- queda con el pico más alto. Un minuto sin tasa (el agent estuvo caído) no
-- está en metrics_1m_networks, así que no baja el promedio.
-- name: AggregateNetworksByHour :execrows
INSERT INTO metrics_1h_networks (
    server_id, bucket, interface, rx_avg_per_sec, rx_max_per_sec, tx_avg_per_sec, tx_max_per_sec
)
SELECT server_id,
       date_trunc('hour', bucket, 'UTC'),
       interface,
       LEAST(avg(rx_avg_per_sec), max(rx_max_per_sec)),
       max(rx_max_per_sec),
       LEAST(avg(tx_avg_per_sec), max(tx_max_per_sec)),
       max(tx_max_per_sec)
FROM metrics_1m_networks
WHERE server_id = sqlc.arg('server_id')::uuid
  AND bucket >= sqlc.arg('from_ts')::timestamptz
  AND bucket < sqlc.arg('to_ts')::timestamptz
GROUP BY server_id, date_trunc('hour', bucket, 'UTC'), interface
ON CONFLICT (server_id, bucket, interface) DO UPDATE SET
    rx_avg_per_sec = EXCLUDED.rx_avg_per_sec,
    rx_max_per_sec = EXCLUDED.rx_max_per_sec,
    tx_avg_per_sec = EXCLUDED.tx_avg_per_sec,
    tx_max_per_sec = EXCLUDED.tx_max_per_sec;
