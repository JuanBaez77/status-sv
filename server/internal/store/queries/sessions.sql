-- CreateSession abre una sesión. El instante lo pone la aplicación, igual que
-- los vencimientos, para que los plazos se midan todos con el mismo reloj.
-- name: CreateSession :one
INSERT INTO sessions (id, user_id, token_hash, created_at, last_seen_at, expires_at, ip, user_agent, stage)
VALUES (sqlc.arg('id'), sqlc.arg('user_id'), sqlc.arg('token_hash'), sqlc.arg('created_at'),
        sqlc.arg('created_at'), sqlc.arg('expires_at'), sqlc.arg('ip'), sqlc.arg('user_agent'), sqlc.arg('stage'))
RETURNING *;

-- GetActiveSession busca la sesión de un token si sigue viva: ni pasó su
-- vencimiento absoluto ni se usó por última vez antes de idle_cutoff.
-- name: GetActiveSession :one
SELECT s.id, s.user_id, s.created_at, s.last_seen_at, s.expires_at, s.stage, s.attempts, u.username
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = sqlc.arg('token_hash')
  AND s.expires_at > sqlc.arg('now')::timestamptz
  AND s.last_seen_at > sqlc.arg('idle_cutoff')::timestamptz;

-- TouchSession renueva el uso de una sesión. Se llama como mucho una vez por
-- minuto por sesión, para no escribir en cada pedido.
-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = sqlc.arg('now') WHERE id = sqlc.arg('id');

-- name: DeleteSessionByTokenHash :execrows
DELETE FROM sessions WHERE token_hash = sqlc.arg('token_hash');

-- DeleteUserSessions cierra todas las sesiones de un usuario, por ejemplo al
-- cambiar la contraseña.
-- name: DeleteUserSessions :execrows
DELETE FROM sessions WHERE user_id = sqlc.arg('user_id');

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions
WHERE expires_at <= sqlc.arg('now')::timestamptz
   OR last_seen_at <= sqlc.arg('idle_cutoff')::timestamptz;

-- CountSessionAttempt anota un código equivocado en una sesión a medias y
-- devuelve cuántos lleva.
-- name: CountSessionAttempt :one
UPDATE sessions SET attempts = attempts + 1 WHERE id = sqlc.arg('id') RETURNING attempts;

-- name: DeleteSession :execrows
DELETE FROM sessions WHERE id = sqlc.arg('id');
