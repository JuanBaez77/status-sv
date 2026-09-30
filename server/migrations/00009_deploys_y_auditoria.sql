-- Deploys y auditoría (ADR-015 y ADR-017).
--
--   deploys     un deploy o un rollback pedido desde el panel, con la última
--               foto que mandó el agent (deploy_event). Es la historia: no se
--               borra cuando el proyecto deja de estar declarado.
--   audit_log   quién hizo qué, sobre qué y cómo salió. Solo inserciones: un
--               trigger rechaza UPDATE, DELETE y TRUNCATE, y además se le
--               sacan esos permisos al rol de la aplicación.

-- +goose Up

CREATE TABLE deploys (
    -- El ID es el del comando que se le mandó al agent: el agent lo usa para
    -- no ejecutar dos veces el mismo pedido y lo devuelve en cada foto.
    id                uuid PRIMARY KEY,
    server_id         uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    -- Si el proyecto deja de estar declarado, su fila se borra pero la
    -- historia queda, con el nombre.
    project_id        uuid        REFERENCES projects (id) ON DELETE SET NULL,
    project_name      text        NOT NULL,
    environment       text        NOT NULL,
    kind              text        NOT NULL,
    -- requested_commit vacío es la punta de la rama declarada.
    requested_commit  text        NOT NULL DEFAULT '',
    deployed_commit   text        NOT NULL DEFAULT '',
    previous_commit   text        NOT NULL DEFAULT '',
    status            text        NOT NULL,
    -- code es el código del deploy (unhealthy, build_failed…) o el del
    -- rechazo del agent (busy, disabled…); message, su explicación.
    code              text        NOT NULL DEFAULT '',
    message           text        NOT NULL DEFAULT '',
    requested_by      uuid        REFERENCES users (id) ON DELETE SET NULL,
    requested_by_name text        NOT NULL,
    requested_at      timestamptz NOT NULL,
    started_at        timestamptz,
    finished_at       timestamptz,
    -- seq es la última foto aplicada; una más vieja se ignora.
    seq               integer     NOT NULL DEFAULT 0,
    steps             jsonb       NOT NULL DEFAULT '[]',
    updated_at        timestamptz NOT NULL,
    CONSTRAINT deploys_kind_valid CHECK (kind IN ('deploy', 'rollback')),
    CONSTRAINT deploys_status_valid CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'rolled_back')),
    CONSTRAINT deploys_environment_valid CHECK (environment IN ('production', 'staging')),
    CONSTRAINT deploys_commits_valid CHECK (
        requested_commit ~ '^([0-9a-f]{40})?$'
        AND deployed_commit ~ '^([0-9a-f]{40})?$'
        AND previous_commit ~ '^([0-9a-f]{40})?$'),
    CONSTRAINT deploys_lengths CHECK (
        length(project_name) BETWEEN 1 AND 63
        AND length(code) <= 64
        AND length(message) <= 1024
        AND length(requested_by_name) BETWEEN 1 AND 64),
    CONSTRAINT deploys_seq_valid CHECK (seq >= 0),
    CONSTRAINT deploys_steps_array CHECK (jsonb_typeof(steps) = 'array'),
    CONSTRAINT deploys_finished_consistent CHECK (
        (status IN ('pending', 'running')) = (finished_at IS NULL))
);

-- Un solo deploy activo por proyecto (ADR-017): dos pedidos simultáneos no
-- pueden ganar los dos, aunque lleguen a la vez.
CREATE UNIQUE INDEX deploys_one_active_per_project
    ON deploys (server_id, project_name)
    WHERE status IN ('pending', 'running');

-- La historia de un proyecto, del más nuevo al más viejo.
CREATE INDEX deploys_history ON deploys (server_id, project_name, requested_at DESC);

CREATE TABLE audit_log (
    id          uuid PRIMARY KEY,
    at          timestamptz NOT NULL,
    -- actor es el nombre de quien actuó: el usuario del panel, el token del
    -- MCP (Fase 3), el servidor del agent o el control plane mismo.
    actor       text        NOT NULL,
    actor_type  text        NOT NULL,
    action      text        NOT NULL,
    target_type text        NOT NULL,
    target_id   text        NOT NULL,
    result      text        NOT NULL,
    ip          text        NOT NULL DEFAULT '',
    detail      jsonb       NOT NULL DEFAULT '{}',
    CONSTRAINT audit_log_actor_type_valid CHECK (actor_type IN ('user', 'llm', 'agent', 'system')),
    CONSTRAINT audit_log_lengths CHECK (
        length(actor) BETWEEN 1 AND 128
        AND length(action) BETWEEN 1 AND 64
        AND length(target_type) BETWEEN 1 AND 32
        AND length(target_id) BETWEEN 1 AND 128
        AND length(result) BETWEEN 1 AND 32
        AND length(ip) <= 64),
    CONSTRAINT audit_log_detail_object CHECK (jsonb_typeof(detail) = 'object'),
    CONSTRAINT audit_log_detail_size CHECK (length(detail::text) <= 4096)
);

CREATE INDEX audit_log_by_time ON audit_log (at DESC);
CREATE INDEX audit_log_by_target ON audit_log (target_type, target_id, at DESC);

-- +goose StatementBegin
CREATE FUNCTION audit_log_insert_only() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_log es solo de inserción: % no está permitido', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_log_no_update_or_delete
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_insert_only();

CREATE TRIGGER audit_log_no_truncate
    BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_insert_only();

-- Además del trigger, el rol de la aplicación pierde esos permisos. El dueño
-- de la tabla podría devolvérselos, pero ningún camino del código lo hace.
REVOKE UPDATE, DELETE, TRUNCATE ON audit_log FROM PUBLIC;
REVOKE UPDATE, DELETE, TRUNCATE ON audit_log FROM CURRENT_USER;

-- +goose Down

DROP TABLE audit_log;
DROP FUNCTION audit_log_insert_only();
DROP TABLE deploys;
