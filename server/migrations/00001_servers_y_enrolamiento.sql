-- Primera migración: lo mínimo para que un agent se enrole y mande su latido.
-- Los IDs son UUID v7 generados por la aplicación (PostgreSQL 17 todavía no trae uuidv7()).
-- Los secretos nunca se guardan en claro: solo su hash SHA-256 de 32 bytes.

-- +goose Up

-- Un registro por VPS enrolado. El estado (online, offline, degradado) no se
-- guarda: se deriva de last_seen_at al consultar, como define docs/DISENO.md.
CREATE TABLE servers (
    id            uuid PRIMARY KEY,
    name          text NOT NULL,
    hostname      text NOT NULL,
    labels        jsonb NOT NULL DEFAULT '{}'::jsonb,
    agent_version text NOT NULL DEFAULT '',
    last_seen_at  timestamptz,
    enrolled_at   timestamptz NOT NULL DEFAULT now(),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT servers_name_not_empty CHECK (length(btrim(name)) > 0)
);

-- El nombre es único sin distinguir mayúsculas: vps-01 y VPS-01 son el mismo servidor.
CREATE UNIQUE INDEX servers_name_key ON servers (lower(name));

-- Token de un solo uso para enrolar un agent. Vence a los 15 minutos (lo impone
-- la aplicación al crearlo) y queda consumido al usarse.
CREATE TABLE enrollment_tokens (
    id          uuid PRIMARY KEY,
    token_hash  bytea NOT NULL,
    server_name text NOT NULL,
    expires_at  timestamptz NOT NULL,
    created_by  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    used_at     timestamptz,
    server_id   uuid REFERENCES servers (id) ON DELETE SET NULL,
    CONSTRAINT enrollment_tokens_hash_is_32_bytes CHECK (octet_length(token_hash) = 32),
    CONSTRAINT enrollment_tokens_server_name_not_empty CHECK (length(btrim(server_name)) > 0),
    -- Un token usado siempre apunta al servidor que creó, y uno sin usar nunca lo hace.
    CONSTRAINT enrollment_tokens_use_is_consistent CHECK ((used_at IS NULL) = (server_id IS NULL))
);

CREATE UNIQUE INDEX enrollment_tokens_token_hash_key ON enrollment_tokens (token_hash);

-- Para el barrido periódico de tokens vencidos sin usar.
CREATE INDEX enrollment_tokens_expires_at_idx ON enrollment_tokens (expires_at) WHERE used_at IS NULL;

-- Credencial propia de cada agent, emitida al enrolarse y revocable de a una.
CREATE TABLE agent_credentials (
    id             uuid PRIMARY KEY,
    server_id      uuid NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    secret_hash    bytea NOT NULL,
    issued_at      timestamptz NOT NULL DEFAULT now(),
    last_used_at   timestamptz,
    revoked_at     timestamptz,
    revoked_reason text,
    CONSTRAINT agent_credentials_hash_is_32_bytes CHECK (octet_length(secret_hash) = 32),
    CONSTRAINT agent_credentials_reason_only_if_revoked CHECK (revoked_reason IS NULL OR revoked_at IS NOT NULL)
);

CREATE UNIQUE INDEX agent_credentials_secret_hash_key ON agent_credentials (secret_hash);

-- Cada servidor tiene como mucho una credencial activa: emitir una nueva obliga
-- a revocar la anterior en la misma transacción.
CREATE UNIQUE INDEX agent_credentials_one_active_per_server ON agent_credentials (server_id) WHERE revoked_at IS NULL;

-- +goose Down

DROP TABLE agent_credentials;
DROP TABLE enrollment_tokens;
DROP TABLE servers;
