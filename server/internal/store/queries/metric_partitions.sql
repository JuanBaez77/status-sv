-- EnsureMetricPartitions crea las particiones que falten para cubrir el rango y
-- devuelve cuántas creó. La lógica vive en una función de la migración 00002,
-- que escapa los nombres de las tablas: desde Go no se arma DDL con texto.
-- name: EnsureMetricPartitions :one
SELECT metrics_ensure_partitions(
    sqlc.arg('parent')::text,
    sqlc.arg('from_ts')::timestamptz,
    sqlc.arg('to_ts')::timestamptz
)::integer AS created;

-- DropMetricPartitions borra las particiones que terminan antes de before_ts y
-- devuelve cuántas borró.
-- name: DropMetricPartitions :one
SELECT metrics_drop_partitions(
    sqlc.arg('parent')::text,
    sqlc.arg('before_ts')::timestamptz
)::integer AS dropped;
