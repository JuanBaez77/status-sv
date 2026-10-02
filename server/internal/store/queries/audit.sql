-- ListAudit lee la auditoría del más nuevo al más viejo (ADR-020). Cada filtro
-- es opcional. El cursor es el instante y el ID del último renglón que ya se
-- vio: así un renglón que entra mientras tanto no corre los que siguen.
-- name: ListAudit :many
SELECT id, at, actor, actor_type, action, target_type, target_id, result, ip, detail
FROM audit_log
WHERE (sqlc.narg('actor_type')::text IS NULL OR actor_type = sqlc.narg('actor_type'))
  AND (sqlc.narg('action')::text IS NULL OR action = sqlc.narg('action'))
  AND (sqlc.narg('target_type')::text IS NULL OR target_type = sqlc.narg('target_type'))
  AND (sqlc.narg('target_id')::text IS NULL OR target_id = sqlc.narg('target_id'))
  AND (sqlc.narg('result')::text IS NULL OR result = sqlc.narg('result'))
  AND (sqlc.narg('from_at')::timestamptz IS NULL OR at >= sqlc.narg('from_at'))
  AND (sqlc.narg('to_at')::timestamptz IS NULL OR at < sqlc.narg('to_at'))
  AND (sqlc.narg('before_at')::timestamptz IS NULL
       OR (at, id) < (sqlc.narg('before_at')::timestamptz, sqlc.narg('before_id')::uuid))
ORDER BY at DESC, id DESC
LIMIT sqlc.arg('max_rows');
