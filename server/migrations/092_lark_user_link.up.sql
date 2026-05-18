-- Lark (Feishu) integration — per-user account link.
-- See LARK_INTEGRATION_DESIGN.md (Phase 2).
--
-- One row per multica user that has linked their Lark account. The link
-- maps the multica user_id to a Lark open_id (which is what card-action
-- callbacks and @bot events identify the actor with).
--
-- refresh_token_enc is the user-level OAuth refresh token, AES-GCM-encrypted
-- with LARK_ENCRYPT_KEY at the application layer. Stored so we can mint
-- fresh user_access_tokens later (P3 docs fetched as the user, future "send
-- as me" actions) without re-prompting OAuth. May be empty when the user
-- only needs identification (open_id) and never needs to act AS the user.
CREATE TABLE lark_user_link (
    user_id           UUID PRIMARY KEY REFERENCES "user"(id) ON DELETE CASCADE,
    lark_open_id      TEXT NOT NULL UNIQUE,
    refresh_token_enc BYTEA NOT NULL DEFAULT ''::bytea,
    linked_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
