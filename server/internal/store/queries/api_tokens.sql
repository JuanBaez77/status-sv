-- Los tokens del MCP (ADR-025). Los instantes los pone la aplicación, como en
-- las sesiones, para que los plazos se midan con el mismo reloj.

-- name: CreateAPIToken :one
INSERT INTO api_tokens (id, name, prefix, token_hash, scopes, project_ids, created_by,
                        created_by_name, created_at, expires_at)
VALUES (sqlc.arg('id'), sqlc.arg('name'), sqlc.arg('prefix'), sqlc.arg('token_hash'),
        sqlc.arg('scopes')::text[], sqlc.arg('project_ids')::uuid[], sqlc.narg('created_by'),
        sqlc.arg('created_by_name'), sqlc.arg('created_at'), sqlc.arg('expires_at'))
RETURNING *;

-- GetActiveAPIToken busca el token de un hash si sigue vivo: sin revocar y
-- sin vencer.
-- name: GetActiveAPIToken :one
SELECT * FROM api_tokens
WHERE token_hash = sqlc.arg('token_hash')
  AND revoked_at IS NULL
  AND expires_at > sqlc.arg('now')::timestamptz;

-- TouchAPIToken anota el último uso, como mucho una vez por minuto: la
-- condición evita escribir en cada pedido.
-- name: TouchAPIToken :execrows
UPDATE api_tokens
SET last_used_at = sqlc.arg('now')::timestamptz, last_used_ip = sqlc.arg('ip')::text
WHERE id = sqlc.arg('id')
  AND (last_used_at IS NULL
       OR last_used_at <= sqlc.arg('now')::timestamptz - interval '1 minute'
       OR last_used_ip <> sqlc.arg('ip'));

-- name: ListAPITokens :many
SELECT * FROM api_tokens ORDER BY created_at DESC, id DESC;

-- name: GetAPIToken :one
SELECT * FROM api_tokens WHERE id = sqlc.arg('id');

-- RevokeAPIToken revoca un token que todavía no lo estaba. Uno vencido se
-- puede revocar igual: así queda claro en el panel que no se usa más.
-- name: RevokeAPIToken :one
UPDATE api_tokens
SET revoked_at = sqlc.arg('now')::timestamptz, revoked_by_name = sqlc.arg('revoked_by_name')::text
WHERE id = sqlc.arg('id') AND revoked_at IS NULL
RETURNING *;

-- RevokeAllAPITokens revoca todos los que siguen sin revocar. Es el freno de
-- emergencia de `server token-revoke --all`.
-- name: RevokeAllAPITokens :execrows
UPDATE api_tokens
SET revoked_at = sqlc.arg('now')::timestamptz, revoked_by_name = sqlc.arg('revoked_by_name')::text
WHERE revoked_at IS NULL;

-- ExistingProjectIDs devuelve cuáles de los IDs son proyectos declarados hoy,
-- para no guardar un token con una lista que no sirve.
-- name: ExistingProjectIDs :many
SELECT id FROM projects WHERE id = ANY(sqlc.arg('ids')::uuid[]);
