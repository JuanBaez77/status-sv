-- InsertRawSamples guarda un lote de muestras de un servidor, una fila por
-- muestra. Cada columna llega como un arreglo, todos del mismo largo: una sola
-- sentencia por lote, sin armar SQL con texto. PostgreSQL recorre los unnest
-- en paralelo; si un arreglo fuera más corto, faltarían valores y la fila
-- chocaría contra un NOT NULL, así que un error de armado no pasa en silencio.
-- Lo que ya estaba (un lote reenviado) se ignora, y el resultado dice cuántas
-- filas eran nuevas.
-- name: InsertRawSamples :execrows
INSERT INTO metrics_raw (
    server_id, ts, cpu_used_percent, cpu_iowait_percent, cpu_steal_percent, cpu_cores,
    mem_total_bytes, mem_used_bytes, swap_total_bytes, swap_used_bytes, load1, load5, load15
)
SELECT sqlc.arg('server_id')::uuid,
       unnest(sqlc.arg('ts')::timestamptz[]),
       unnest(sqlc.arg('cpu_used')::real[]),
       unnest(sqlc.arg('cpu_iowait')::real[]),
       unnest(sqlc.arg('cpu_steal')::real[]),
       unnest(sqlc.arg('cpu_cores')::smallint[]),
       unnest(sqlc.arg('mem_total')::bigint[]),
       unnest(sqlc.arg('mem_used')::bigint[]),
       unnest(sqlc.arg('swap_total')::bigint[]),
       unnest(sqlc.arg('swap_used')::bigint[]),
       unnest(sqlc.arg('load1')::real[]),
       unnest(sqlc.arg('load5')::real[]),
       unnest(sqlc.arg('load15')::real[])
ON CONFLICT DO NOTHING;

-- InsertRawDisks guarda los discos de un lote, una fila por disco y por muestra.
-- name: InsertRawDisks :execrows
INSERT INTO metrics_raw_disks (server_id, ts, mountpoint, total_bytes, used_bytes, available_bytes)
SELECT sqlc.arg('server_id')::uuid,
       unnest(sqlc.arg('ts')::timestamptz[]),
       unnest(sqlc.arg('mountpoint')::text[]),
       unnest(sqlc.arg('total')::bigint[]),
       unnest(sqlc.arg('used')::bigint[]),
       unnest(sqlc.arg('available')::bigint[])
ON CONFLICT DO NOTHING;

-- InsertRawNetworks guarda las interfaces de un lote, una fila por interfaz y
-- por muestra.
-- name: InsertRawNetworks :execrows
INSERT INTO metrics_raw_networks (server_id, ts, interface, rx_bytes, tx_bytes)
SELECT sqlc.arg('server_id')::uuid,
       unnest(sqlc.arg('ts')::timestamptz[]),
       unnest(sqlc.arg('interface')::text[]),
       unnest(sqlc.arg('rx')::bigint[]),
       unnest(sqlc.arg('tx')::bigint[])
ON CONFLICT DO NOTHING;
