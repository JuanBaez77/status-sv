-- Lo que el agent informa de un proyecto dado de alta (ADR-022 y ADR-023), para
-- que el panel lo muestre: el sitio de nginx, si están cargadas las variables
-- requeridas, la llave pública de la deploy key y el hash del project.yaml.
--
-- De las variables viajan nombres y cuántas son, nunca valores. La llave es la
-- pública: la privada no sale del servidor.

-- +goose Up

ALTER TABLE projects
    ADD COLUMN site_domain  text    NOT NULL DEFAULT '',
    ADD COLUMN site_port    integer NOT NULL DEFAULT 0,
    -- env_required es cuántas variables declara el proyecto; cero es que no
    -- declara ninguna, y entonces env_missing y env_unknown no dicen nada.
    ADD COLUMN env_required integer NOT NULL DEFAULT 0,
    ADD COLUMN env_missing  text[]  NOT NULL DEFAULT '{}',
    -- env_unknown es que el agent no pudo mirar (con systemd lo mira el
    -- ayudante, que puede no estar).
    ADD COLUMN env_unknown  boolean NOT NULL DEFAULT false,
    ADD COLUMN deploy_key   text    NOT NULL DEFAULT '',
    ADD COLUMN spec_hash    text    NOT NULL DEFAULT '',
    ADD CONSTRAINT projects_site_valid CHECK ((site_domain = '') = (site_port = 0)),
    ADD CONSTRAINT projects_env_valid CHECK (
        env_required BETWEEN 0 AND 20 AND cardinality(env_missing) <= env_required
        AND NOT (env_unknown AND cardinality(env_missing) > 0)),
    ADD CONSTRAINT projects_spec_hash_valid CHECK (spec_hash = '' OR spec_hash ~ '^[0-9a-f]{64}$');

-- +goose Down

ALTER TABLE projects
    DROP CONSTRAINT projects_spec_hash_valid,
    DROP CONSTRAINT projects_env_valid,
    DROP CONSTRAINT projects_site_valid,
    DROP COLUMN spec_hash,
    DROP COLUMN deploy_key,
    DROP COLUMN env_unknown,
    DROP COLUMN env_missing,
    DROP COLUMN env_required,
    DROP COLUMN site_port,
    DROP COLUMN site_domain;
