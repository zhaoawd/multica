-- =====================
-- Lark workspace binding
-- =====================

-- name: GetLarkWorkspaceBinding :one
SELECT * FROM lark_workspace_binding
WHERE workspace_id = $1;

-- name: ListLarkWorkspaceBindings :many
SELECT * FROM lark_workspace_binding
ORDER BY created_at ASC;

-- name: UpsertLarkWorkspaceBinding :one
INSERT INTO lark_workspace_binding (
    workspace_id, chat_id, bot_token_enc, enabled_events
) VALUES (
    $1, $2, $3, $4
)
ON CONFLICT (workspace_id) DO UPDATE SET
    chat_id        = EXCLUDED.chat_id,
    bot_token_enc  = EXCLUDED.bot_token_enc,
    enabled_events = EXCLUDED.enabled_events,
    updated_at     = now()
RETURNING *;

-- name: UpdateLarkWorkspaceBindingEvents :one
UPDATE lark_workspace_binding
SET enabled_events = $2,
    updated_at = now()
WHERE workspace_id = $1
RETURNING *;

-- name: DeleteLarkWorkspaceBinding :exec
DELETE FROM lark_workspace_binding WHERE workspace_id = $1;
