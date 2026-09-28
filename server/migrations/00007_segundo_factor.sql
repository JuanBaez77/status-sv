-- Segundo factor TOTP del panel (ADR-013).
--
-- Del secreto TOTP se guarda solo su versión cifrada con AES-256-GCM, con una
-- clave que vive en un archivo fuera de la base. totp_pending_secret es el del
-- alta en curso: recién pasa a totp_secret cuando la persona lo confirma con un
-- código. totp_last_step es el último paso aceptado, para que un código no sirva
-- dos veces.
--
-- Las sesiones pasan a tener etapa: una contraseña correcta abre una sesión a
-- medias (esperando el código, o el alta si todavía no hay TOTP), que solo sirve
-- para eso; el código correcto la cambia por una completa con otro token.
--
-- De los códigos de recuperación se guarda el SHA-256, como de los tokens: son
-- 80 bits al azar.

-- +goose Up

ALTER TABLE users
    ADD COLUMN totp_secret         bytea,
    ADD COLUMN totp_pending_secret bytea,
    ADD COLUMN totp_last_step      bigint NOT NULL DEFAULT 0,
    ADD COLUMN totp_enabled_at     timestamptz,
    ADD CONSTRAINT users_totp_enabled_consistent CHECK ((totp_secret IS NULL) = (totp_enabled_at IS NULL));

-- Las sesiones abiertas antes de esta migración entraron sin segundo factor:
-- se cierran todas, y el próximo login pasa por el código o por el alta.
DELETE FROM sessions;

ALTER TABLE sessions
    ADD COLUMN stage    text     NOT NULL,
    ADD COLUMN attempts smallint NOT NULL DEFAULT 0,
    ADD CONSTRAINT sessions_stage_valid CHECK (stage IN ('full', 'totp_pending', 'totp_enroll')),
    ADD CONSTRAINT sessions_attempts_not_negative CHECK (attempts >= 0);

CREATE TABLE user_recovery_codes (
    id         uuid PRIMARY KEY,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash  bytea       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    used_at    timestamptz,
    CONSTRAINT user_recovery_codes_hash_is_32_bytes CHECK (octet_length(code_hash) = 32)
);

CREATE UNIQUE INDEX user_recovery_codes_user_hash_key ON user_recovery_codes (user_id, code_hash);

-- +goose Down

DROP TABLE user_recovery_codes;

-- Sin la columna de etapa, una sesión a medias pasaría por completa.
DELETE FROM sessions;

ALTER TABLE sessions
    DROP COLUMN attempts,
    DROP COLUMN stage;

ALTER TABLE users
    DROP CONSTRAINT users_totp_enabled_consistent,
    DROP COLUMN totp_enabled_at,
    DROP COLUMN totp_last_step,
    DROP COLUMN totp_pending_secret,
    DROP COLUMN totp_secret;
