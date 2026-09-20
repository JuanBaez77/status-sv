-- name: CreateEnrollmentToken :one
INSERT INTO enrollment_tokens (id, token_hash, server_name, expires_at, created_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetEnrollmentTokenByHash :one
SELECT * FROM enrollment_tokens WHERE token_hash = $1;

-- ConsumeEnrollmentToken marca el token como usado y lo ata al servidor que creó.
-- La condición used_at IS NULL hace que el segundo intento no devuelva ninguna
-- fila, así que un token no se puede usar dos veces ni aunque dos agents lo
-- presenten a la vez.
-- name: ConsumeEnrollmentToken :one
UPDATE enrollment_tokens
SET used_at   = now(),
    server_id = sqlc.arg('server_id')
WHERE token_hash = sqlc.arg('token_hash')
  AND used_at IS NULL
  AND expires_at > now()
RETURNING *;

-- name: ListPendingEnrollmentTokens :many
SELECT * FROM enrollment_tokens
WHERE used_at IS NULL AND expires_at > now()
ORDER BY created_at DESC;

-- name: DeleteExpiredEnrollmentTokens :execrows
DELETE FROM enrollment_tokens
WHERE used_at IS NULL AND expires_at <= now();
