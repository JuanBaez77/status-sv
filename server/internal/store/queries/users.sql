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
