-- Los reinicios en la historia de deploys (ADR-026).
--
-- Un reinicio corre en el motor de deploy del agent, de a uno con los deploys,
-- y queda en la misma historia. No trae código: no tiene commit pedido ni
-- anterior; su deployed_commit es el que corre, si el agent lo sabe.

-- +goose Up

ALTER TABLE deploys
    DROP CONSTRAINT deploys_kind_valid,
    ADD CONSTRAINT deploys_kind_valid CHECK (kind IN ('deploy', 'rollback', 'bootstrap', 'restart')),
    ADD CONSTRAINT deploys_restart_has_no_commits CHECK (
        kind <> 'restart' OR (requested_commit = '' AND previous_commit = '' AND status <> 'rolled_back'));

-- +goose Down

-- La historia de los reinicios se pierde al volver atrás: el esquema anterior
-- no los conoce.
DELETE FROM deploys WHERE kind = 'restart';

ALTER TABLE deploys
    DROP CONSTRAINT deploys_restart_has_no_commits,
    DROP CONSTRAINT deploys_kind_valid,
    ADD CONSTRAINT deploys_kind_valid CHECK (kind IN ('deploy', 'rollback', 'bootstrap'));
