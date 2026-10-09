-- Propuestas de proyectos nuevos (ADR-022, migración 00011).

-- SupersedeProposal cierra la propuesta abierta con ese nombre en ese
-- servidor, si hay una: la reemplaza la nueva.
-- name: SupersedeProposal :many
UPDATE project_proposals
SET status = 'superseded', closed_at = sqlc.arg('at')::timestamptz
WHERE server_id = sqlc.arg('server_id') AND name = sqlc.arg('name') AND status IN ('pending', 'delivered')
RETURNING id;

-- CreateProposal anota una propuesta nueva.
-- name: CreateProposal :one
INSERT INTO project_proposals (
    id, server_id, name, runtime, environment, spec, spec_hash, status,
    created_by, created_by_name, actor_type, created_at, expires_at
) VALUES (
    sqlc.arg('id'), sqlc.arg('server_id'), sqlc.arg('name'), sqlc.arg('runtime'), sqlc.arg('environment'),
    sqlc.arg('spec'), sqlc.arg('spec_hash'), 'pending',
    sqlc.narg('created_by'), sqlc.arg('created_by_name'), sqlc.arg('actor_type'),
    sqlc.arg('created_at'), sqlc.arg('expires_at')
)
RETURNING *;

-- GetProposal devuelve una propuesta con el nombre de su servidor.
-- name: GetProposal :one
SELECT p.*, s.name AS server_name
FROM project_proposals p
JOIN servers s ON s.id = p.server_id
WHERE p.id = sqlc.arg('id');

-- ListServerProposals devuelve las propuestas de un servidor, de la más nueva
-- a la más vieja.
-- name: ListServerProposals :many
SELECT * FROM project_proposals
WHERE server_id = sqlc.arg('server_id')
ORDER BY created_at DESC
LIMIT sqlc.arg('max_rows');

-- OpenProposals devuelve las propuestas abiertas de un servidor, que el agent
-- tiene que tener guardadas.
-- name: OpenProposals :many
SELECT * FROM project_proposals
WHERE server_id = sqlc.arg('server_id') AND status IN ('pending', 'delivered') AND expires_at > sqlc.arg('now')
ORDER BY created_at;

-- MarkProposalDelivered anota que el agent la guardó.
-- name: MarkProposalDelivered :execrows
UPDATE project_proposals
SET status = 'delivered', delivered_at = COALESCE(delivered_at, sqlc.arg('at')::timestamptz)
WHERE id = sqlc.arg('id') AND server_id = sqlc.arg('server_id') AND status = 'pending';

-- CloseProposal cierra una propuesta abierta: rechazada por el agent,
-- retirada, vencida o aceptada.
-- name: CloseProposal :one
UPDATE project_proposals
SET status = sqlc.arg('status'), message = sqlc.arg('message'), closed_at = sqlc.arg('at')::timestamptz
WHERE id = sqlc.arg('id') AND status IN ('pending', 'delivered')
RETURNING *;

-- ExpireProposals cierra las propuestas abiertas que vencieron.
-- name: ExpireProposals :many
UPDATE project_proposals
SET status = 'expired', closed_at = sqlc.arg('now')::timestamptz
WHERE status IN ('pending', 'delivered') AND expires_at <= sqlc.arg('now')
RETURNING *;

-- KnownProposals dice, de una lista de IDs, cuáles son propuestas abiertas de
-- ese servidor: las demás, el agent las tiene que borrar.
-- name: KnownProposals :many
SELECT id FROM project_proposals
WHERE server_id = sqlc.arg('server_id') AND id = ANY (sqlc.arg('ids')::uuid[])
  AND status IN ('pending', 'delivered') AND expires_at > sqlc.arg('now');

-- ProjectDeclaredOnServer dice si ese servidor ya declara un proyecto con ese
-- nombre: entonces no se puede proponer.
-- name: ProjectDeclaredOnServer :one
SELECT EXISTS (SELECT 1 FROM projects WHERE server_id = sqlc.arg('server_id') AND name = sqlc.arg('name'));
