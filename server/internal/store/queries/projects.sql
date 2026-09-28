-- Proyectos declarados (ADR-012, migración 00008).

-- AdvanceProjectsTakenAt avanza la marca de la última lectura de proyectos de
-- un servidor, solo si la nueva es más reciente, y guarda cómo salió leer el
-- directorio. Cero filas es una lectura vieja que no hay que aplicar.
-- name: AdvanceProjectsTakenAt :execrows
UPDATE servers
SET projects_taken_at = sqlc.arg('taken_at'),
    projects_status   = sqlc.arg('status'),
    projects_detail   = sqlc.arg('detail')
WHERE id = sqlc.arg('server_id')
  AND (projects_taken_at IS NULL OR projects_taken_at < sqlc.arg('taken_at'));

-- DeleteProjectsExcept borra los proyectos de un servidor que ya no están
-- declarados. Con la lista vacía borra todos.
-- name: DeleteProjectsExcept :execrows
DELETE FROM projects
WHERE server_id = sqlc.arg('server_id')
  AND NOT (name = ANY (sqlc.arg('keep')::text[]));

-- UpsertProject guarda un proyecto. Si ya existía con ese nombre en ese
-- servidor conserva su ID, que es el que usa el panel.
-- name: UpsertProject :exec
INSERT INTO projects (
    id, server_id, name, environment, repo, branch, runtime, workdir, compose_file, compose_project,
    units, healthcheck_url, healthcheck_expect_status, healthcheck_timeout_seconds,
    health_state, health_checked_at, health_status_code, health_latency_ms, health_detail, seen_at
) VALUES (
    sqlc.arg('id'), sqlc.arg('server_id'), sqlc.arg('name'), sqlc.arg('environment'), sqlc.arg('repo'),
    sqlc.arg('branch'), sqlc.arg('runtime'), sqlc.arg('workdir'), sqlc.arg('compose_file'),
    sqlc.arg('compose_project'), sqlc.arg('units')::text[], sqlc.arg('healthcheck_url'),
    sqlc.arg('healthcheck_expect_status'), sqlc.arg('healthcheck_timeout_seconds'),
    sqlc.arg('health_state'), sqlc.narg('health_checked_at'), sqlc.arg('health_status_code'),
    sqlc.arg('health_latency_ms'), sqlc.arg('health_detail'), sqlc.arg('seen_at')
)
ON CONFLICT (server_id, name) DO UPDATE
SET environment                 = EXCLUDED.environment,
    repo                        = EXCLUDED.repo,
    branch                      = EXCLUDED.branch,
    runtime                     = EXCLUDED.runtime,
    workdir                     = EXCLUDED.workdir,
    compose_file                = EXCLUDED.compose_file,
    compose_project             = EXCLUDED.compose_project,
    units                       = EXCLUDED.units,
    healthcheck_url             = EXCLUDED.healthcheck_url,
    healthcheck_expect_status   = EXCLUDED.healthcheck_expect_status,
    healthcheck_timeout_seconds = EXCLUDED.healthcheck_timeout_seconds,
    health_state                = EXCLUDED.health_state,
    health_checked_at           = EXCLUDED.health_checked_at,
    health_status_code          = EXCLUDED.health_status_code,
    health_latency_ms           = EXCLUDED.health_latency_ms,
    health_detail               = EXCLUDED.health_detail,
    seen_at                     = EXCLUDED.seen_at;

-- DeleteInvalidProjectFiles borra los archivos inválidos de un servidor, antes
-- de guardar los de la lectura nueva.
-- name: DeleteInvalidProjectFiles :execrows
DELETE FROM project_invalid_files WHERE server_id = sqlc.arg('server_id');

-- InsertInvalidProjectFiles guarda los archivos inválidos de una lectura.
-- name: InsertInvalidProjectFiles :execrows
INSERT INTO project_invalid_files (server_id, file, error, seen_at)
SELECT sqlc.arg('server_id')::uuid, unnest(sqlc.arg('files')::text[]), unnest(sqlc.arg('errors')::text[]),
       sqlc.arg('seen_at')::timestamptz;

-- ListProjects devuelve todos los proyectos, con el nombre de su servidor, en
-- orden de servidor y de nombre.
-- name: ListProjects :many
SELECT p.*, s.name AS server_name
FROM projects p
JOIN servers s ON s.id = p.server_id
ORDER BY s.name, p.name;

-- GetProject devuelve un proyecto con el nombre de su servidor.
-- name: GetProject :one
SELECT p.*, s.name AS server_name
FROM projects p
JOIN servers s ON s.id = p.server_id
WHERE p.id = sqlc.arg('id');

-- ListInvalidProjectFiles devuelve los archivos que no se pudieron usar, de
-- todos los servidores.
-- name: ListInvalidProjectFiles :many
SELECT f.server_id, s.name AS server_name, f.file, f.error, f.seen_at
FROM project_invalid_files f
JOIN servers s ON s.id = f.server_id
ORDER BY s.name, f.file;

-- ListProjectReadErrors devuelve los servidores cuya última lectura del
-- directorio de proyectos falló: lo que se muestra de ellos puede estar viejo.
-- name: ListProjectReadErrors :many
SELECT id, name, projects_detail, projects_taken_at
FROM servers
WHERE projects_status = 'error'
ORDER BY name;
