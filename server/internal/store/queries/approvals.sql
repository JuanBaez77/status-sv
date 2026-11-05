-- Los pedidos de escritura del MCP y su aprobación (ADR-026). Los instantes los
-- pone la aplicación, para que el vencimiento se mida con el mismo reloj.

-- name: CreateApproval :exec
INSERT INTO approvals (id, operation, server_id, project_id, project_name, environment, commit_sha, unit,
                       reason, requested_by_token, requested_by_name, requested_by_prefix, requested_ip,
                       requested_at, expires_at, status)
VALUES (sqlc.arg('id'), sqlc.arg('operation'), sqlc.arg('server_id'), sqlc.narg('project_id'),
        sqlc.arg('project_name'), sqlc.arg('environment'), sqlc.arg('commit_sha'), sqlc.arg('unit'),
        sqlc.arg('reason'), sqlc.narg('requested_by_token'), sqlc.arg('requested_by_name'),
        sqlc.arg('requested_by_prefix'), sqlc.arg('requested_ip'), sqlc.arg('requested_at'),
        sqlc.arg('expires_at'), 'pending');

-- name: GetApproval :one
SELECT a.*, s.name AS server_name
FROM approvals a JOIN servers s ON s.id = a.server_id
WHERE a.id = sqlc.arg('id');

-- PendingApprovalForProject es el pedido que espera sobre un proyecto, para
-- nombrarlo cuando llega otro.
-- name: PendingApprovalForProject :one
SELECT id FROM approvals WHERE project_id = sqlc.arg('project_id') AND status = 'pending';

-- ListApprovals devuelve los pedidos, los más nuevos primero. Con status vacío,
-- de todos los estados.
-- name: ListApprovals :many
SELECT a.*, s.name AS server_name
FROM approvals a JOIN servers s ON s.id = a.server_id
WHERE sqlc.arg('status')::text = '' OR a.status = sqlc.arg('status')::text
ORDER BY a.requested_at DESC, a.id DESC
LIMIT sqlc.arg('max_rows');

-- DecideApproval aprueba o rechaza un pedido que sigue pendiente y no venció.
-- Sin filas es que ya se había decidido o que venció.
-- name: DecideApproval :one
UPDATE approvals
SET status = sqlc.arg('status')::text, decided_at = sqlc.arg('now')::timestamptz,
    decided_by = sqlc.narg('decided_by'), decided_by_name = sqlc.arg('decided_by_name')::text,
    decision_note = sqlc.arg('decision_note')::text
WHERE id = sqlc.arg('id') AND status = 'pending' AND expires_at > sqlc.arg('now')::timestamptz
RETURNING id;

-- CancelApproval cancela un pedido pendiente: el sistema lo decide, sin
-- persona.
-- name: CancelApproval :one
UPDATE approvals
SET status = 'cancelled', decided_at = sqlc.arg('now')::timestamptz, decision_note = sqlc.arg('note')::text
WHERE id = sqlc.arg('id') AND status = 'pending'
RETURNING id;

-- ExpireApprovals vence los pendientes cuyo plazo pasó.
-- name: ExpireApprovals :many
UPDATE approvals
SET status = 'expired', decided_at = sqlc.arg('now')::timestamptz, decision_note = 'venció sin que nadie lo decidiera'
WHERE status = 'pending' AND expires_at <= sqlc.arg('now')::timestamptz
RETURNING id;

-- CancelApprovalsOfDeadTokens cancela los pendientes cuyo token se revocó,
-- venció o ya no está: un pedido no sobrevive a la credencial que lo hizo.
-- name: CancelApprovalsOfDeadTokens :many
UPDATE approvals a
SET status = 'cancelled', decided_at = sqlc.arg('now')::timestamptz,
    decision_note = 'el token que lo pidió fue revocado o venció'
WHERE a.status = 'pending'
  AND (a.requested_by_token IS NULL
       OR EXISTS (SELECT 1 FROM api_tokens t
                  WHERE t.id = a.requested_by_token
                    AND (t.revoked_at IS NOT NULL OR t.expires_at <= sqlc.arg('now')::timestamptz)))
RETURNING a.id;

-- CancelApprovalsWithoutProject cancela los pendientes de un proyecto que dejó
-- de estar declarado.
-- name: CancelApprovalsWithoutProject :many
UPDATE approvals
SET status = 'cancelled', decided_at = sqlc.arg('now')::timestamptz,
    decision_note = 'el proyecto dejó de estar declarado'
WHERE status = 'pending' AND project_id IS NULL
RETURNING id;

-- SetApprovalDeploy anota el deploy que salió de un pedido aprobado.
-- name: SetApprovalDeploy :execrows
UPDATE approvals SET deploy_id = sqlc.arg('deploy_id')
WHERE id = sqlc.arg('id') AND status = 'approved' AND deploy_id IS NULL;
