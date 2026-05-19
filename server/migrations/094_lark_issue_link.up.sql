-- Lark (Feishu) integration — issue ↔ thread bridge state (Phase 4).
-- See LARK_INTEGRATION_DESIGN.md §5.2 / §6.5.
--
-- One row per multica issue that was created from a Lark thread via an
-- @bot 创建任务 command (or any future verb that opens a long-running
-- discussion). The link records the chat the thread lives in and the
-- root message id of the thread, so:
--
--   1. When an agent posts a comment on the issue, the P5 bridge can
--      look the link up and reply into the original Lark thread.
--   2. When a human replies in the Lark thread, the inbound webhook
--      handler can map the thread back to the originating issue and
--      append the reply as an issue comment.
--
-- chat_id   — the open_chat_id the thread lives in. Matches
--             lark_workspace_binding.chat_id when the thread is in the
--             workspace's bound chat (the common case), but is stored
--             on the link itself so cross-workspace forwarding (a
--             future feature) doesn't require a re-derivation.
-- root_message_id — the open_message_id of the thread's first message
--             (or the @bot-mention message that opened the issue when
--             the conversation started fresh). All P5 replies attach
--             to this id via Lark's /im/v1/messages/:id/reply endpoint.
CREATE TABLE lark_issue_link (
    issue_id        UUID PRIMARY KEY REFERENCES issue(id) ON DELETE CASCADE,
    chat_id         TEXT NOT NULL,
    root_message_id TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Reverse lookup for the inbound bridge: given a Lark message that
-- replied to a known root, find the multica issue to comment on.
CREATE INDEX lark_issue_link_root_message_id_idx
    ON lark_issue_link (root_message_id);
