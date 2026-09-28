-- Códigos de recuperación del segundo factor (ADR-013). Se guarda el SHA-256
-- del código normalizado, y cada uno sirve una vez.

-- name: InsertRecoveryCodes :execrows
INSERT INTO user_recovery_codes (id, user_id, code_hash)
SELECT unnest(sqlc.arg('ids')::uuid[]), sqlc.arg('user_id')::uuid, unnest(sqlc.arg('hashes')::bytea[]);

-- name: DeleteRecoveryCodes :execrows
DELETE FROM user_recovery_codes WHERE user_id = sqlc.arg('user_id');

-- UseRecoveryCode marca un código como usado, si existe y no se usó.
-- name: UseRecoveryCode :execrows
UPDATE user_recovery_codes SET used_at = sqlc.arg('now')::timestamptz
WHERE user_id = sqlc.arg('user_id') AND code_hash = sqlc.arg('code_hash') AND used_at IS NULL;

-- name: CountUnusedRecoveryCodes :one
SELECT count(*) FROM user_recovery_codes WHERE user_id = sqlc.arg('user_id') AND used_at IS NULL;
