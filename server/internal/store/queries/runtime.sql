-- AdvanceRuntimeTakenAt avanza la marca de la última foto de un servidor, solo
-- si la nueva es más reciente. Cero filas quiere decir que la foto es vieja (un
-- reenvío que llegó tarde) y no hay que aplicarla.
-- name: AdvanceRuntimeTakenAt :execrows
UPDATE servers
SET runtime_taken_at = sqlc.arg('taken_at')
WHERE id = sqlc.arg('server_id')
  AND (runtime_taken_at IS NULL OR runtime_taken_at < sqlc.arg('taken_at'));

-- UpsertRuntimeSource guarda el resultado de leer una fuente.
-- name: UpsertRuntimeSource :exec
INSERT INTO runtime_sources (server_id, source, status, detail, omitted, taken_at)
VALUES (sqlc.arg('server_id'), sqlc.arg('source'), sqlc.arg('status'), sqlc.arg('detail'),
        sqlc.arg('omitted'), sqlc.arg('taken_at'))
ON CONFLICT (server_id, source) DO UPDATE
SET status   = EXCLUDED.status,
    detail   = EXCLUDED.detail,
    omitted  = EXCLUDED.omitted,
    taken_at = EXCLUDED.taken_at;

-- DeleteRuntimeUnitsExcept borra las unidades de un tipo que ya no están en la
-- foto. Con la lista vacía borra todas las de ese tipo.
-- name: DeleteRuntimeUnitsExcept :execrows
DELETE FROM runtime_units
WHERE server_id = sqlc.arg('server_id')
  AND kind = sqlc.arg('kind')
  AND NOT (unit_key = ANY (sqlc.arg('keep')::text[]));

-- UpsertRuntimeUnits guarda las unidades de un tipo, una fila por unidad, con
-- cada columna como un arreglo (como la ingesta de métricas). Lo que falta
-- viaja como un valor que no puede ocurrir y se vuelve NULL acá: el año 1 para
-- las fechas y -1 para el consumo.
-- name: UpsertRuntimeUnits :execrows
INSERT INTO runtime_units (
    server_id, kind, unit_key, name, state, health, load_state, sub_state, result, image,
    compose_project, compose_service, pid, user_name, command, started_at,
    cpu_percent, memory_bytes, seen_at
)
SELECT sqlc.arg('server_id')::uuid,
       sqlc.arg('kind')::text,
       unnest(sqlc.arg('unit_key')::text[]),
       unnest(sqlc.arg('name')::text[]),
       unnest(sqlc.arg('state')::text[]),
       unnest(sqlc.arg('health')::text[]),
       unnest(sqlc.arg('load_state')::text[]),
       unnest(sqlc.arg('sub_state')::text[]),
       unnest(sqlc.arg('result')::text[]),
       unnest(sqlc.arg('image')::text[]),
       unnest(sqlc.arg('compose_project')::text[]),
       unnest(sqlc.arg('compose_service')::text[]),
       unnest(sqlc.arg('pid')::integer[]),
       unnest(sqlc.arg('user_name')::text[]),
       unnest(sqlc.arg('command')::text[]),
       NULLIF(unnest(sqlc.arg('started_at')::timestamptz[]), '0001-01-01 00:00:00+00'::timestamptz),
       NULLIF(unnest(sqlc.arg('cpu_percent')::real[]), -1),
       NULLIF(unnest(sqlc.arg('memory_bytes')::bigint[]), -1),
       sqlc.arg('seen_at')::timestamptz
ON CONFLICT (server_id, kind, unit_key) DO UPDATE
SET name            = EXCLUDED.name,
    state           = EXCLUDED.state,
    health          = EXCLUDED.health,
    load_state      = EXCLUDED.load_state,
    sub_state       = EXCLUDED.sub_state,
    result          = EXCLUDED.result,
    image           = EXCLUDED.image,
    compose_project = EXCLUDED.compose_project,
    compose_service = EXCLUDED.compose_service,
    pid             = EXCLUDED.pid,
    user_name       = EXCLUDED.user_name,
    command         = EXCLUDED.command,
    started_at      = EXCLUDED.started_at,
    cpu_percent     = EXCLUDED.cpu_percent,
    memory_bytes    = EXCLUDED.memory_bytes,
    seen_at         = EXCLUDED.seen_at;

-- ListRuntimeSources devuelve el estado de las fuentes de un servidor.
-- name: ListRuntimeSources :many
SELECT source, status, detail, omitted, taken_at
FROM runtime_sources
WHERE server_id = sqlc.arg('server_id')
ORDER BY source;

-- ListRuntimeUnits devuelve las unidades de un servidor, agrupadas por tipo y
-- en orden de nombre.
-- name: ListRuntimeUnits :many
SELECT kind, unit_key, name, state, health, load_state, sub_state, result, image,
       compose_project, compose_service, pid, user_name, command, started_at,
       cpu_percent, memory_bytes, seen_at
FROM runtime_units
WHERE server_id = sqlc.arg('server_id')
ORDER BY kind, name, unit_key;
