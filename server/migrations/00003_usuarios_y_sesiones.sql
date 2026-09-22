-- Usuarios del panel y sus sesiones (ADR-010).
--
-- status-sv es single-tenant (ADR-005): los usuarios son las personas que
-- operan la plataforma, sin roles. El primero lo crea Juan por SSH con
-- `server user-add`, que pide la contraseña sin mostrarla.
--
-- De la contraseña se guarda solo el hash argon2id, en el formato PHC
-- ($argon2id$v=19$m=...,t=...,p=...$sal$hash), que lleva sus parámetros: se
-- pueden subir más adelante sin romper los hashes viejos. De la sesión se guarda
-- solo el SHA-256 del token, como con las credenciales de los agents.

-- +goose Up

CREATE TABLE users (
    id                  uuid PRIMARY KEY,
    username            text        NOT NULL,
    password_hash       text        NOT NULL,
    password_changed_at timestamptz NOT NULL DEFAULT now(),
    created_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_username_valid CHECK (username ~ '^[a-z0-9][a-z0-9._-]{2,31}$'),
    CONSTRAINT users_password_hash_is_argon2id CHECK (password_hash LIKE '$argon2id$%')
);

-- La aplicación guarda el nombre en minúsculas; la restricción lo exige, así que
-- el índice único alcanza para que juan y Juan sean el mismo.
CREATE UNIQUE INDEX users_username_key ON users (username);

-- Una sesión vence por lo que ocurra primero: 24 h sin uso (last_seen_at) o
-- 7 días desde que se abrió (expires_at). La aplicación decide los plazos; la
-- base guarda los instantes.
CREATE TABLE sessions (
    id           uuid PRIMARY KEY,
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   bytea       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    ip           text        NOT NULL,
    user_agent   text        NOT NULL,
    CONSTRAINT sessions_hash_is_32_bytes CHECK (octet_length(token_hash) = 32),
    CONSTRAINT sessions_expires_after_creation CHECK (expires_at > created_at),
    CONSTRAINT sessions_ip_length CHECK (length(ip) <= 64),
    CONSTRAINT sessions_user_agent_length CHECK (length(user_agent) <= 512)
);

CREATE UNIQUE INDEX sessions_token_hash_key ON sessions (token_hash);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
-- Para el barrido de sesiones vencidas.
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- +goose Down

DROP TABLE sessions;
DROP TABLE users;
