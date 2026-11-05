-- Los pedidos de escritura del MCP y su aprobación (ADR-026).
--
-- Una herramienta de escritura no le manda nada al agent: guarda acá un pedido
-- pendiente, con sus parámetros tipados. Una persona lo aprueba en el panel, con
-- TOTP, o lo rechaza; si nadie lo decide, vence a la hora. Lo que se ejecuta
-- sale de estas columnas, nunca de reason, que es texto del LLM.
--
-- Un pedido decidido no cambia más, salvo para anotar el deploy que salió de
-- él. La fila no se borra: la auditoría la nombra.

-- +goose Up

CREATE TABLE approvals (
    id                     uuid PRIMARY KEY,
    -- operation es lo que se pide: deploy, rollback, restart o bootstrap.
    operation              text        NOT NULL,
    server_id              uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    -- Si el proyecto deja de estar declarado, el pedido queda con su nombre.
    project_id             uuid        REFERENCES projects (id) ON DELETE SET NULL,
    project_name           text        NOT NULL,
    environment            text        NOT NULL,
    -- commit_sha es el SHA completo que se aprueba y se despliega; solo en deploy.
    commit_sha             text        NOT NULL DEFAULT '',
    -- unit es la unidad del proyecto a reiniciar; solo en restart, y vacía es
    -- todas las del proyecto.
    unit                   text        NOT NULL DEFAULT '',
    -- reason es el motivo que dio el LLM: texto no confiable, solo se muestra.
    reason                 text        NOT NULL DEFAULT '',
    -- El token que lo pidió. Revocado o vencido, el pedido se cancela; si la
    -- fila del token se borrara, el pedido queda con su nombre y su prefijo.
    requested_by_token     uuid        REFERENCES api_tokens (id) ON DELETE SET NULL,
    requested_by_name      text        NOT NULL,
    requested_by_prefix    text        NOT NULL,
    requested_ip           text        NOT NULL,
    requested_at           timestamptz NOT NULL,
    expires_at             timestamptz NOT NULL,
    status                 text        NOT NULL,
    decided_at             timestamptz,
    -- decided_by es la persona que aprobó o rechazó; un pedido vencido o
    -- cancelado lo decide el sistema y queda sin persona.
    decided_by             uuid        REFERENCES users (id) ON DELETE SET NULL,
    decided_by_name        text,
    -- decision_note dice por qué se rechazó o se canceló.
    decision_note          text        NOT NULL DEFAULT '',
    -- deploy_id es el deploy que salió de un pedido aprobado.
    deploy_id              uuid        REFERENCES deploys (id) ON DELETE SET NULL,
    CONSTRAINT approvals_operation_valid CHECK (operation IN ('deploy', 'rollback', 'restart', 'bootstrap')),
    CONSTRAINT approvals_environment_valid CHECK (environment IN ('production', 'staging')),
    CONSTRAINT approvals_status_valid CHECK (status IN ('pending', 'approved', 'rejected', 'expired', 'cancelled')),
    -- Un deploy lleva su commit completo, y nada más lo lleva.
    CONSTRAINT approvals_commit_valid CHECK (
        (operation = 'deploy' AND commit_sha ~ '^[0-9a-f]{40}$')
        OR (operation <> 'deploy' AND commit_sha = '')),
    CONSTRAINT approvals_unit_valid CHECK (
        length(unit) <= 255 AND (operation = 'restart' OR unit = '')),
    CONSTRAINT approvals_lengths CHECK (
        length(project_name) BETWEEN 1 AND 63
        AND length(reason) <= 500
        AND length(requested_by_name) BETWEEN 1 AND 64
        AND requested_by_prefix ~ '^svmcp_[A-Za-z0-9_-]{6}$'
        AND length(requested_ip) <= 64
        AND length(decision_note) <= 512),
    -- Vence a la hora (ADR-026); la base no deja guardar uno más largo.
    CONSTRAINT approvals_expiry_valid CHECK (
        expires_at > requested_at AND expires_at <= requested_at + interval '1 hour'),
    -- Pendiente es sin decidir; decidido es con fecha, y con persona solo si
    -- lo aprobó o lo rechazó alguien.
    CONSTRAINT approvals_decision_consistent CHECK (
        (status = 'pending') = (decided_at IS NULL)
        AND (status IN ('approved', 'rejected')) = (decided_by_name IS NOT NULL)
        AND (decided_by_name IS NULL OR length(decided_by_name) BETWEEN 1 AND 64)),
    CONSTRAINT approvals_deploy_only_if_approved CHECK (deploy_id IS NULL OR status = 'approved')
);

-- Un pedido pendiente por proyecto: otro pedido sobre el mismo proyecto se
-- rechaza y nombra el que está esperando.
CREATE UNIQUE INDEX approvals_one_pending_per_project ON approvals (project_id) WHERE status = 'pending';

CREATE INDEX approvals_requested_at ON approvals (requested_at DESC);
CREATE INDEX approvals_pending_expiry ON approvals (expires_at) WHERE status = 'pending';
CREATE INDEX approvals_token ON approvals (requested_by_token) WHERE status = 'pending';

-- +goose Down

DROP TABLE approvals;
