-- name: CreateUser :one
INSERT INTO users (id, username, password_hash)
VALUES (sqlc.arg('id'), sqlc.arg('username'), sqlc.arg('password_hash'))
RETURNING *;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = sqlc.arg('username');

-- name: GetUserByID :one
SELECT * FROM users WHERE id = sqlc.arg('id');

-- SetUserPassword cambia el hash. Lo usan el cambio de contraseña y la
-- actualización silenciosa de parámetros de argon2id al iniciar sesión.
-- name: SetUserPassword :execrows
UPDATE users
SET password_hash = sqlc.arg('password_hash'),
    password_changed_at = CASE WHEN sqlc.arg('changed')::boolean THEN now() ELSE password_changed_at END
WHERE id = sqlc.arg('id');

-- name: CountUsers :one
SELECT count(*) FROM users;

-- SetPendingTOTP guarda el secreto cifrado de un alta en curso. Pisa el de un
-- alta anterior que no se confirmó.
-- name: SetPendingTOTP :execrows
UPDATE users SET totp_pending_secret = sqlc.arg('pending') WHERE id = sqlc.arg('id') AND totp_secret IS NULL;

-- EnableTOTP confirma el alta: el secreto pendiente pasa a ser el del usuario.
-- Solo si el pendiente sigue siendo el que se verificó, por si otra pestaña
-- empezó otra alta en el medio.
-- name: EnableTOTP :execrows
UPDATE users
SET totp_secret = totp_pending_secret,
    totp_pending_secret = NULL,
    totp_last_step = sqlc.arg('step'),
    totp_enabled_at = sqlc.arg('now')::timestamptz
WHERE id = sqlc.arg('id')
  AND totp_secret IS NULL
  AND totp_pending_secret = sqlc.arg('pending');

-- AdvanceTOTPStep anota el paso de un código aceptado. Si ya se usó ese paso o
-- uno posterior, no cambia nada: dos pedidos con el mismo código a la vez no
-- pasan los dos.
-- name: AdvanceTOTPStep :execrows
UPDATE users SET totp_last_step = sqlc.arg('step')
WHERE id = sqlc.arg('id') AND totp_last_step < sqlc.arg('step');

-- ResetTOTP borra el segundo factor de un usuario: el próximo login pide el alta.
-- name: ResetTOTP :execrows
UPDATE users
SET totp_secret = NULL, totp_pending_secret = NULL, totp_last_step = 0, totp_enabled_at = NULL
WHERE id = sqlc.arg('id');
