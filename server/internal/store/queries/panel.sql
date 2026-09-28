-- Consultas del panel. El estado (online, offline) no se guarda: sale de
-- last_seen_at al consultar, como define docs/DISENO.md.

-- ListLatestSamples devuelve la última muestra de cada servidor que mandó algo
-- desde since, para la lista del panel. El corte por since evita recorrer
-- particiones viejas cuando un servidor está caído hace rato.
--
-- Va aparte de la lista de servidores, y no como LEFT JOIN, porque un servidor
-- puede no tener ninguna muestra: unir en Go es más simple que arrastrar una
-- fila entera de columnas que pueden faltar.
-- name: ListLatestSamples :many
SELECT DISTINCT ON (server_id)
       server_id, ts, cpu_used_percent, cpu_iowait_percent, cpu_steal_percent, cpu_cores,
       mem_total_bytes, mem_used_bytes, swap_total_bytes, swap_used_bytes, load1, load5, load15
FROM metrics_raw
WHERE ts >= sqlc.arg('since')::timestamptz
ORDER BY server_id, ts DESC;

-- ListLatestWorstDisks devuelve, por servidor, el disco más lleno de su última
-- muestra. La fracción usada se calcula al armar la respuesta. Es lo que el
-- panel muestra en la lista: si
-- un disco se está llenando, se ve sin entrar al servidor.
-- name: ListLatestWorstDisks :many
SELECT DISTINCT ON (server_id)
       server_id, ts, mountpoint, total_bytes, used_bytes, available_bytes
FROM metrics_raw_disks
WHERE ts >= sqlc.arg('since')::timestamptz AND total_bytes > 0
ORDER BY server_id, ts DESC, used_bytes::float8 / total_bytes DESC;

-- name: ListRawSamples :many
SELECT ts, cpu_used_percent, cpu_iowait_percent, cpu_steal_percent, cpu_cores,
       mem_total_bytes, mem_used_bytes, swap_total_bytes, swap_used_bytes, load1, load5, load15
FROM metrics_raw
WHERE server_id = sqlc.arg('server_id')
  AND ts >= sqlc.arg('from_ts')::timestamptz
  AND ts < sqlc.arg('to_ts')::timestamptz
ORDER BY ts
LIMIT sqlc.arg('max_rows');

-- name: ListRawDisks :many
SELECT ts, mountpoint, total_bytes, used_bytes, available_bytes
FROM metrics_raw_disks
WHERE server_id = sqlc.arg('server_id')
  AND ts >= sqlc.arg('from_ts')::timestamptz
  AND ts < sqlc.arg('to_ts')::timestamptz
ORDER BY mountpoint, ts
LIMIT sqlc.arg('max_rows');

-- ListRawNetworks trae los contadores acumulados. Las tasas se calculan al
-- armar la respuesta, con las mismas reglas que el agregado por minuto: un
-- contador que baja o un hueco largo no son una tasa.
-- name: ListRawNetworks :many
SELECT ts, interface, rx_bytes, tx_bytes
FROM metrics_raw_networks
WHERE server_id = sqlc.arg('server_id')
  AND ts >= sqlc.arg('from_ts')::timestamptz
  AND ts < sqlc.arg('to_ts')::timestamptz
ORDER BY interface, ts
LIMIT sqlc.arg('max_rows');

-- name: ListMinuteSamples :many
SELECT bucket, samples, cpu_used_avg, cpu_used_max, cpu_iowait_avg, cpu_iowait_max, cpu_steal_avg,
       cpu_cores, mem_total_bytes, mem_used_avg_bytes, mem_used_max_bytes,
       swap_total_bytes, swap_used_avg_bytes, swap_used_max_bytes, load1_avg, load5_avg, load15_avg
FROM metrics_1m
WHERE server_id = sqlc.arg('server_id')
  AND bucket >= sqlc.arg('from_ts')::timestamptz
  AND bucket < sqlc.arg('to_ts')::timestamptz
ORDER BY bucket
LIMIT sqlc.arg('max_rows');

-- name: ListMinuteDisks :many
SELECT bucket, mountpoint, total_bytes, used_max_bytes, available_min_bytes
FROM metrics_1m_disks
WHERE server_id = sqlc.arg('server_id')
  AND bucket >= sqlc.arg('from_ts')::timestamptz
  AND bucket < sqlc.arg('to_ts')::timestamptz
ORDER BY mountpoint, bucket
LIMIT sqlc.arg('max_rows');

-- name: ListMinuteNetworks :many
SELECT bucket, interface, rx_avg_per_sec, rx_max_per_sec, tx_avg_per_sec, tx_max_per_sec
FROM metrics_1m_networks
WHERE server_id = sqlc.arg('server_id')
  AND bucket >= sqlc.arg('from_ts')::timestamptz
  AND bucket < sqlc.arg('to_ts')::timestamptz
ORDER BY interface, bucket
LIMIT sqlc.arg('max_rows');

-- name: ListHourSamples :many
SELECT bucket, samples, cpu_used_min, cpu_used_avg, cpu_used_max, cpu_iowait_avg, cpu_iowait_max, cpu_steal_avg,
       cpu_cores, mem_total_bytes, mem_used_min_bytes, mem_used_avg_bytes, mem_used_max_bytes,
       swap_total_bytes, swap_used_avg_bytes, swap_used_max_bytes, load1_avg, load5_avg, load15_avg
FROM metrics_1h
WHERE server_id = sqlc.arg('server_id')
  AND bucket >= sqlc.arg('from_ts')::timestamptz
  AND bucket < sqlc.arg('to_ts')::timestamptz
ORDER BY bucket
LIMIT sqlc.arg('max_rows');

-- name: ListHourDisks :many
SELECT bucket, mountpoint, total_bytes, used_max_bytes, available_min_bytes
FROM metrics_1h_disks
WHERE server_id = sqlc.arg('server_id')
  AND bucket >= sqlc.arg('from_ts')::timestamptz
  AND bucket < sqlc.arg('to_ts')::timestamptz
ORDER BY mountpoint, bucket
LIMIT sqlc.arg('max_rows');

-- name: ListHourNetworks :many
SELECT bucket, interface, rx_avg_per_sec, rx_max_per_sec, tx_avg_per_sec, tx_max_per_sec
FROM metrics_1h_networks
WHERE server_id = sqlc.arg('server_id')
  AND bucket >= sqlc.arg('from_ts')::timestamptz
  AND bucket < sqlc.arg('to_ts')::timestamptz
ORDER BY interface, bucket
LIMIT sqlc.arg('max_rows');
