-- El primer deploy de un proyecto (ADR-021) es un tipo más de deploy: comparte
-- la tabla, la historia, el avance en vivo y el bloqueo de simultáneos.
--
-- No tiene un estado anterior: no lleva commit pedido (es siempre la punta de
-- la rama declarada) ni commit anterior.

-- +goose Up

ALTER TABLE deploys
    DROP CONSTRAINT deploys_kind_valid,
    ADD CONSTRAINT deploys_kind_valid CHECK (kind IN ('deploy', 'rollback', 'bootstrap')),
    ADD CONSTRAINT deploys_bootstrap_has_no_previous CHECK (
        kind <> 'bootstrap' OR (requested_commit = '' AND previous_commit = ''));

-- +goose Down

-- La historia de los primeros deploys se pierde al volver atrás: el esquema
-- anterior no tiene dónde guardarla.
DELETE FROM deploys WHERE kind = 'bootstrap';

ALTER TABLE deploys
    DROP CONSTRAINT deploys_bootstrap_has_no_previous,
    DROP CONSTRAINT deploys_kind_valid,
    ADD CONSTRAINT deploys_kind_valid CHECK (kind IN ('deploy', 'rollback'));
