-- =====================
-- Lark workspace binding
-- =====================

-- name: GetLarkWorkspaceBinding :one
SELECT * FROM lark_workspace_binding
WHERE workspace_id = $1;

-- name: GetLarkWorkspaceBindingByChatID :one
SELECT * FROM lark_workspace_binding
WHERE chat_id = $1;

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

-- name: MarkLarkBindingPermWarning :exec
-- Stamps last_perm_warning_at = now() for the workspace. Called once
-- per binding after the §14.1.3 "missing im:resource scope" bot reply
-- has been posted into the thread; the renderer reads the column to
-- avoid re-posting on every subsequent attachment failure.
UPDATE lark_workspace_binding
SET last_perm_warning_at = now()
WHERE workspace_id = $1;

-- =====================
-- Lark user link
-- =====================

-- name: GetLarkUserLink :one
SELECT * FROM lark_user_link
WHERE user_id = $1;

-- name: GetLarkUserLinkByOpenID :one
SELECT * FROM lark_user_link
WHERE lark_open_id = $1;

-- name: UpsertLarkUserLink :one
INSERT INTO lark_user_link (
    user_id, lark_open_id, refresh_token_enc
) VALUES (
    $1, $2, $3
)
ON CONFLICT (user_id) DO UPDATE SET
    lark_open_id      = EXCLUDED.lark_open_id,
    refresh_token_enc = EXCLUDED.refresh_token_enc,
    linked_at         = now()
RETURNING *;

-- name: DeleteLarkUserLink :exec
DELETE FROM lark_user_link WHERE user_id = $1;

-- =====================
-- Lark issue link (P4)
-- =====================

-- name: InsertLarkIssueLink :one
INSERT INTO lark_issue_link (
    issue_id, chat_id, root_message_id
) VALUES (
    $1, $2, $3
)
ON CONFLICT (issue_id) DO UPDATE SET
    chat_id         = EXCLUDED.chat_id,
    root_message_id = EXCLUDED.root_message_id
RETURNING *;

-- name: GetLarkIssueLinkByIssueID :one
SELECT * FROM lark_issue_link
WHERE issue_id = $1;

-- name: GetLarkIssueLinkByRootMessage :one
SELECT * FROM lark_issue_link
WHERE root_message_id = $1;

-- name: DeleteLarkIssueLink :exec
DELETE FROM lark_issue_link WHERE issue_id = $1;
