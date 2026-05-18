-- Lark (Feishu) integration — per-workspace chat binding.
-- See LARK_INTEGRATION_DESIGN.md (Phase 1).
--
-- chat_id is the Lark open_chat_id the bot posts notifications into.
-- bot_token_enc holds the workspace-specific bot token (where applicable),
--   AES-GCM-encrypted with LARK_ENCRYPT_KEY at the application layer. May be
--   empty when the deployment uses a single app-level access token derived from
--   LARK_APP_ID / LARK_APP_SECRET (the P1 default).
-- enabled_events is the allowlist of multica event types (e.g.
--   "issue:created") that should fan out to this chat. An empty array means
--   the binding exists but no events are forwarded (manual mute).
CREATE TABLE lark_workspace_binding (
    workspace_id   UUID PRIMARY KEY REFERENCES workspace(id) ON DELETE CASCADE,
    chat_id        TEXT NOT NULL,
    bot_token_enc  BYTEA NOT NULL DEFAULT ''::bytea,
    enabled_events TEXT[] NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
