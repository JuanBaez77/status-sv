-- Deploys y auditoría (ADR-015 y ADR-017, migración 00009).

-- CreateDeploy anota un deploy pedido, antes de mandárselo al agent. Si ya
-- hay uno activo en el proyecto, el índice único lo rechaza.
-- name: CreateDeploy :exec
INSERT INTO deploys (
    id, server_id, project_id, project_name, environment, kind, requested_commit, status,
    requested_by, requested_by_name, requested_at, updated_at
) VALUES (
    sqlc.arg('id'), sqlc.arg('server_id'), sqlc.arg('project_id'), sqlc.arg('project_name'),
    sqlc.arg('environment'), sqlc.arg('kind'), sqlc.arg('requested_commit'), 'pending',
    sqlc.arg('requested_by'), sqlc.arg('requested_by_name'), sqlc.arg('requested_at'), sqlc.arg('requested_at')
);

-- RejectDeploy cierra un deploy que el agent no aceptó (o que no llegó a
-- mandarse), mientras siga pendiente.
-- name: RejectDeploy :one
UPDATE deploys
SET status      = 'failed',
    code        = sqlc.arg('code'),
    message     = sqlc.arg('message'),
    finished_at = sqlc.arg('at')::timestamptz,
    updated_at  = sqlc.arg('at')::timestamptz
WHERE id = sqlc.arg('id') AND status = 'pending'
RETURNING *;

-- ApplyDeployEvent guarda la foto que mandó el agent, solo si es más nueva
-- que la guardada y viene del servidor al que se le pidió el deploy.
--
-- Un deploy que el agent ya cerró no se vuelve a abrir ni cambia de final: su
-- resultado ya está en audit_log. El que cerró el control plane sin haber
-- recibido ninguna foto (seq 0: rechazado o sin respuesta) sí puede revivir,
-- porque el agent pudo haberlo empezado igual.
-- name: ApplyDeployEvent :one
UPDATE deploys
SET status          = sqlc.arg('status'),
    deployed_commit = sqlc.arg('deployed_commit'),
    previous_commit = sqlc.arg('previous_commit'),
    code            = sqlc.arg('code'),
    message         = sqlc.arg('message'),
    started_at      = sqlc.arg('started_at'),
    finished_at     = sqlc.narg('finished_at'),
    seq             = sqlc.arg('seq'),
    steps           = sqlc.arg('steps'),
    updated_at      = sqlc.arg('updated_at')
WHERE id = sqlc.arg('id')
  AND server_id = sqlc.arg('server_id')
  AND seq < sqlc.arg('seq')
  AND (seq = 0 OR status IN ('pending', 'running'))
RETURNING *;

-- DeploySeq devuelve la última foto aplicada de un deploy de un servidor. Sin
-- filas, el deploy no existe o es de otro servidor.
-- name: DeploySeq :one
SELECT seq FROM deploys WHERE id = sqlc.arg('id') AND server_id = sqlc.arg('server_id');

-- ExpirePendingDeploys cierra los deploys que siguen pendientes pasado el
-- plazo: el agent nunca confirmó que los recibió.
-- name: ExpirePendingDeploys :many
UPDATE deploys
SET status      = 'failed',
    code        = 'no_answer',
    message     = 'el agent no confirmó el pedido',
    finished_at = sqlc.arg('now')::timestamptz,
    updated_at  = sqlc.arg('now')::timestamptz
WHERE status = 'pending' AND seq = 0 AND requested_at < sqlc.arg('before')
RETURNING *;

-- GetDeploy devuelve un deploy con el nombre de su servidor.
-- name: GetDeploy :one
SELECT d.*, s.name AS server_name
FROM deploys d
JOIN servers s ON s.id = d.server_id
WHERE d.id = sqlc.arg('id');

-- ListProjectDeploys devuelve la historia de un proyecto, del más nuevo al
-- más viejo, sin los pasos (que se piden con el detalle).
-- name: ListProjectDeploys :many
SELECT id, kind, requested_commit, deployed_commit, previous_commit, status, code, message,
       requested_by_name, requested_at, started_at, finished_at
FROM deploys
WHERE server_id = sqlc.arg('server_id') AND project_name = sqlc.arg('project_name')
ORDER BY requested_at DESC
LIMIT sqlc.arg('max_rows');

-- InsertAudit agrega un renglón a la auditoría. Es la única escritura que
-- existe sobre audit_log.
-- name: InsertAudit :exec
INSERT INTO audit_log (id, at, actor, actor_type, action, target_type, target_id, result, ip, detail)
VALUES (
    sqlc.arg('id'), sqlc.arg('at'), sqlc.arg('actor'), sqlc.arg('actor_type'), sqlc.arg('action'),
    sqlc.arg('target_type'), sqlc.arg('target_id'), sqlc.arg('result'), sqlc.arg('ip'), sqlc.arg('detail')
);
