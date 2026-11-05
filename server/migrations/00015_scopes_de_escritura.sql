-- Los scopes de escritura de los tokens del MCP (ADR-025, ADR-026).
--
-- Un token puede pedir deploys, rollbacks y reinicios por entorno. Pedir no es
-- ejecutar: cada pedido espera la aprobación de una persona en el panel.

-- +goose Up

ALTER TABLE api_tokens
    DROP CONSTRAINT api_tokens_scopes_valid,
    ADD CONSTRAINT api_tokens_scopes_valid CHECK (
        cardinality(scopes) BETWEEN 1 AND 10
        AND scopes <@ ARRAY['read:staging', 'read:production', 'logs:staging', 'logs:production',
                            'deploy:staging', 'deploy:production', 'rollback:staging', 'rollback:production',
                            'restart:staging', 'restart:production']::text[]);

-- +goose Down

-- Un token con escritura no existe en el esquema anterior: se borra. La
-- auditoría lo sigue nombrando, y sus pedidos quedan sin la referencia.
DELETE FROM api_tokens
WHERE NOT scopes <@ ARRAY['read:staging', 'read:production', 'logs:staging', 'logs:production']::text[];

ALTER TABLE api_tokens
    DROP CONSTRAINT api_tokens_scopes_valid,
    ADD CONSTRAINT api_tokens_scopes_valid CHECK (
        cardinality(scopes) BETWEEN 1 AND 4
        AND scopes <@ ARRAY['read:staging', 'read:production', 'logs:staging', 'logs:production']::text[]);
