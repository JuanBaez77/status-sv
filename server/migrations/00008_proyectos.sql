-- Proyectos declarados en cada servidor, como los manda el agent en
-- projects_state (ADR-012).
--
-- El control plane no crea ni cambia proyectos: guarda lo último que el agent
-- leyó de /etc/status-sv/projects.d. Igual que runtime_units, no hay historia:
-- cada lectura reemplaza a la anterior.
--
--   projects                 un proyecto válido por fila.
--   project_invalid_files    los archivos que no se pudieron usar, con el
--                            motivo (nunca el contenido).
--
-- El ligado con los contenedores y servicios no se guarda: se cruza con
-- runtime_units al leer.

-- +goose Up

-- Igual que runtime_taken_at: la marca de la última lectura aplicada y cómo
-- salió leer el directorio. Con error se conserva lo anterior.
ALTER TABLE servers
    ADD COLUMN projects_taken_at timestamptz,
    ADD COLUMN projects_status   text NOT NULL DEFAULT '',
    ADD COLUMN projects_detail   text NOT NULL DEFAULT '',
    ADD CONSTRAINT servers_projects_status_valid CHECK (projects_status IN ('', 'ok', 'unavailable', 'error')),
    ADD CONSTRAINT servers_projects_detail_length CHECK (length(projects_detail) <= 256);

-- El ID es estable mientras el proyecto siga declarado con el mismo nombre en
-- el mismo servidor: es lo que usa el panel en la URL.
CREATE TABLE projects (
    id                          uuid PRIMARY KEY,
    server_id                   uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    name                        text        NOT NULL,
    environment                 text        NOT NULL,
    repo                        text        NOT NULL,
    branch                      text        NOT NULL,
    runtime                     text        NOT NULL,
    workdir                     text        NOT NULL,
    compose_file                text        NOT NULL DEFAULT '',
    compose_project             text        NOT NULL DEFAULT '',
    units                       text[]      NOT NULL DEFAULT '{}',
    -- El healthcheck declarado; la URL vacía es que no declara ninguno.
    healthcheck_url             text        NOT NULL DEFAULT '',
    healthcheck_expect_status   integer     NOT NULL DEFAULT 0,
    healthcheck_timeout_seconds integer     NOT NULL DEFAULT 0,
    -- El último resultado del healthcheck pasivo; vacío es que no hay.
    health_state                text        NOT NULL DEFAULT '',
    health_checked_at           timestamptz,
    health_status_code          integer     NOT NULL DEFAULT 0,
    health_latency_ms           integer     NOT NULL DEFAULT 0,
    health_detail               text        NOT NULL DEFAULT '',
    seen_at                     timestamptz NOT NULL,
    CONSTRAINT projects_name_valid CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    CONSTRAINT projects_environment_valid CHECK (environment IN ('production', 'staging')),
    CONSTRAINT projects_runtime_valid CHECK (runtime IN ('docker', 'systemd')),
    CONSTRAINT projects_lengths CHECK (
        length(repo) BETWEEN 1 AND 256
        AND length(branch) BETWEEN 1 AND 128
        AND length(workdir) BETWEEN 2 AND 256
        AND length(compose_file) <= 256
        AND length(compose_project) <= 128
        AND length(healthcheck_url) <= 512
        AND length(health_detail) <= 128
        AND cardinality(units) <= 10),
    CONSTRAINT projects_health_valid CHECK (health_state IN ('', 'passing', 'failing'))
);

CREATE UNIQUE INDEX projects_server_name_key ON projects (server_id, name);

CREATE TABLE project_invalid_files (
    server_id uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    file      text        NOT NULL,
    error     text        NOT NULL,
    seen_at   timestamptz NOT NULL,
    PRIMARY KEY (server_id, file),
    CONSTRAINT project_invalid_files_lengths CHECK (
        length(file) BETWEEN 1 AND 128 AND length(error) BETWEEN 1 AND 256)
);

-- +goose Down

DROP TABLE project_invalid_files;
DROP TABLE projects;
ALTER TABLE servers
    DROP CONSTRAINT servers_projects_detail_length,
    DROP CONSTRAINT servers_projects_status_valid,
    DROP COLUMN projects_detail,
    DROP COLUMN projects_status,
    DROP COLUMN projects_taken_at;
