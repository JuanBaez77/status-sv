-- Las propuestas de proyectos nuevos (ADR-022).
--
-- El control plane no declara proyectos: los propone. Una propuesta es la
-- declaración completa de un project.yaml, que viaja al agent del servidor y
-- espera ahí a que una persona la acepte con `statussv-agent project accept`.
-- La tabla lleva la cuenta de cada una: si llegó, si se aceptó, si se retiró.
--
-- No guarda nada secreto: la declaración solo tiene los nombres de las
-- variables requeridas, nunca sus valores.

-- +goose Up

CREATE TABLE project_proposals (
    id              uuid PRIMARY KEY,
    server_id       uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    name            text        NOT NULL,
    runtime         text        NOT NULL,
    environment     text        NOT NULL,
    -- spec es el texto del project.yaml, tal cual viaja al agent; spec_hash,
    -- su SHA-256, el mismo que el agent informa del archivo declarado.
    spec            text        NOT NULL,
    spec_hash       text        NOT NULL,
    status          text        NOT NULL DEFAULT 'pending',
    -- message es por qué el agent la rechazó, o cómo se aceptó.
    message         text        NOT NULL DEFAULT '',
    created_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    created_by_name text        NOT NULL,
    actor_type      text        NOT NULL,
    created_at      timestamptz NOT NULL,
    expires_at      timestamptz NOT NULL,
    delivered_at    timestamptz,
    closed_at       timestamptz,

    CONSTRAINT project_proposals_status_valid CHECK (
        status IN ('pending', 'delivered', 'accepted', 'rejected', 'withdrawn', 'expired', 'superseded')),
    CONSTRAINT project_proposals_runtime_valid CHECK (runtime IN ('docker', 'systemd')),
    CONSTRAINT project_proposals_environment_valid CHECK (environment IN ('production', 'staging')),
    CONSTRAINT project_proposals_actor_type_valid CHECK (actor_type IN ('user', 'llm')),
    CONSTRAINT project_proposals_hash_valid CHECK (spec_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT project_proposals_spec_size CHECK (octet_length(spec) <= 32768),
    CONSTRAINT project_proposals_closed CHECK (
        (status IN ('pending', 'delivered')) = (closed_at IS NULL))
);

-- Una propuesta abierta por nombre y servidor: una nueva reemplaza a la
-- anterior, que queda superseded.
CREATE UNIQUE INDEX project_proposals_open ON project_proposals (server_id, name)
    WHERE status IN ('pending', 'delivered');

CREATE INDEX project_proposals_server ON project_proposals (server_id, created_at DESC);

-- +goose Down

DROP TABLE project_proposals;
