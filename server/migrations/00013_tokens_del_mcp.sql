-- Los tokens del MCP (ADR-025).
--
-- Un token es la credencial con la que Claude Code habla con el MCP. Se guarda
-- solo su SHA-256: el valor en claro se muestra una vez, al crearlo, y no está
-- en ningún otro lado. Los primeros caracteres (prefix) sirven para
-- reconocerlo en el panel y en la auditoría, y no alcanzan para usarlo.
--
-- Un token vencido o revocado no se borra: la auditoría lo sigue nombrando.

-- +goose Up

CREATE TABLE api_tokens (
    id              uuid PRIMARY KEY,
    name            text        NOT NULL,
    prefix          text        NOT NULL,
    token_hash      bytea       NOT NULL,
    -- scopes es la lista cerrada de permisos por entorno. El Sprint 8 suma los
    -- de escritura cambiando la restricción.
    scopes          text[]      NOT NULL,
    -- project_ids vacío es que el token alcanza a todos los proyectos de sus
    -- scopes; con IDs, solo a esos. Un ID que deja de existir no se limpia: no
    -- coincide con nada, y el token no pasa a ver todo.
    project_ids     uuid[]      NOT NULL DEFAULT '{}',
    created_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    created_by_name text        NOT NULL,
    created_at      timestamptz NOT NULL,
    expires_at      timestamptz NOT NULL,
    revoked_at      timestamptz,
    revoked_by_name text,
    last_used_at    timestamptz,
    last_used_ip    text,
    CONSTRAINT api_tokens_hash_unique UNIQUE (token_hash),
    CONSTRAINT api_tokens_hash_is_32_bytes CHECK (octet_length(token_hash) = 32),
    CONSTRAINT api_tokens_name_valid CHECK (length(name) BETWEEN 1 AND 64),
    CONSTRAINT api_tokens_prefix_valid CHECK (prefix ~ '^svmcp_[A-Za-z0-9_-]{6}$'),
    CONSTRAINT api_tokens_scopes_valid CHECK (
        cardinality(scopes) BETWEEN 1 AND 4
        AND scopes <@ ARRAY['read:staging', 'read:production', 'logs:staging', 'logs:production']::text[]),
    CONSTRAINT api_tokens_projects_valid CHECK (cardinality(project_ids) <= 50),
    CONSTRAINT api_tokens_created_by_name_valid CHECK (length(created_by_name) BETWEEN 1 AND 64),
    -- Vence siempre, y como mucho a los 90 días.
    CONSTRAINT api_tokens_expiry_valid CHECK (
        expires_at > created_at AND expires_at <= created_at + interval '90 days'),
    CONSTRAINT api_tokens_revoked_valid CHECK (
        (revoked_at IS NULL) = (revoked_by_name IS NULL)
        AND (revoked_by_name IS NULL OR length(revoked_by_name) BETWEEN 1 AND 64)),
    CONSTRAINT api_tokens_last_used_valid CHECK (
        (last_used_at IS NULL) = (last_used_ip IS NULL)
        AND (last_used_ip IS NULL OR length(last_used_ip) <= 64))
);

CREATE INDEX api_tokens_created_at ON api_tokens (created_at DESC);

-- +goose Down

DROP TABLE api_tokens;
