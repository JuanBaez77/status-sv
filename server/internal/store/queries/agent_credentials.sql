-- name: CreateAgentCredential :one
INSERT INTO agent_credentials (id, server_id, secret_hash)
VALUES ($1, $2, $3)
RETURNING *;

-- GetActiveCredentialByHash es la consulta de autenticación del agent: busca por
-- hash y solo devuelve la credencial si no fue revocada.
-- name: GetActiveCredentialByHash :one
SELECT * FROM agent_credentials
WHERE secret_hash = $1 AND revoked_at IS NULL;

-- name: TouchCredentialLastUsed :exec
UPDATE agent_credentials
SET last_used_at = now()
WHERE id = $1;

-- name: RevokeAgentCredential :one
UPDATE agent_credentials
SET revoked_at     = now(),
    revoked_reason = sqlc.arg('reason')
WHERE id = sqlc.arg('id') AND revoked_at IS NULL
RETURNING *;

-- name: RevokeActiveCredentialsForServer :execrows
UPDATE agent_credentials
SET revoked_at     = now(),
    revoked_reason = sqlc.arg('reason')
WHERE server_id = sqlc.arg('server_id') AND revoked_at IS NULL;

-- name: ListCredentialsForServer :many
SELECT * FROM agent_credentials
WHERE server_id = $1
ORDER BY issued_at DESC;
