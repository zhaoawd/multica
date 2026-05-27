-- Lark message references for cards that may be updated in place.
--
-- This starts with issue claim/assigned cards so terminal issue updates can
-- remove stale write actions. The shape intentionally matches the later
-- streaming-card design: stage_or_event is the card/update family and target_id
-- is the concrete chat_id/open_id that received the card.
CREATE TABLE lark_message_ref (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id   UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    issue_id       UUID REFERENCES issue(id) ON DELETE CASCADE,
    stage_or_event TEXT NOT NULL,
    channel        TEXT NOT NULL CHECK (channel IN ('dm', 'team', 'thread')),
    target_id      TEXT NOT NULL,
    message_id     TEXT NOT NULL,
    version        INT NOT NULL DEFAULT 0,
    last_event_ref TEXT,
    status         TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'superseded', 'finalized')),
    superseded_at  TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX lark_message_ref_active_idx
    ON lark_message_ref (issue_id, stage_or_event, channel, target_id)
    WHERE status = 'active';

CREATE INDEX lark_message_ref_issue_active_idx
    ON lark_message_ref (issue_id)
    WHERE status = 'active';
