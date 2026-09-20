-- name: CreateServer :one
INSERT INTO servers (id, name, hostname, labels, agent_version, enrolled_at)
VALUES ($1, $2, $3, $4, $5, now())
RETURNING *;

-- name: GetServerByID :one
SELECT * FROM servers WHERE id = $1;

-- name: GetServerByName :one
SELECT * FROM servers WHERE lower(name) = lower(sqlc.arg('name'));

-- name: ListServers :many
SELECT * FROM servers ORDER BY name;

-- TouchServer deja constancia de que el agent sigue vivo. Es lo que actualiza el
-- latido cada 15 segundos, y de lo que se deriva el estado online u offline.
-- name: TouchServer :one
UPDATE servers
SET last_seen_at  = now(),
    agent_version = COALESCE(sqlc.narg('agent_version'), agent_version),
    updated_at    = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: UpdateServerHostname :one
UPDATE servers
SET hostname   = $2,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteServer :execrows
DELETE FROM servers WHERE id = $1;
