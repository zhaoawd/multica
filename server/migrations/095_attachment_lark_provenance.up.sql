-- Lark thread media → issue attachment (LARK_INTEGRATION_DESIGN.md §14.1.3).
--
-- When `@bot 创建任务` lifts a Lark thread into a multica issue, the
-- thread's images / PDFs are downloaded into multica's blob storage and
-- attached to the new issue (so the agent reading the issue context
-- can see them without ever touching the Lark API). Two columns
-- support that flow on the existing attachment table:
--
--   content_sha256 — workspace-scoped dedup. The same screenshot that
--     two people paste into two different Lark threads should land
--     as one blob with two attachment rows (or, with the unique
--     constraint considered, two rows pointing at the same blob URL).
--     The index is partial to avoid bloat on the historic rows
--     uploaded via the regular `POST /api/upload-file` path which
--     never compute the hash.
--
--   source — provenance. Format: 'lark_thread:<chat_id>:<message_id>'
--     so a forensic question ("where did this image come from") can
--     be answered without a JOIN to the lark_issue_link table. NULL
--     for the historic upload path; populated only by the Lark
--     bridge today.
ALTER TABLE attachment
    ADD COLUMN content_sha256 TEXT,
    ADD COLUMN source         TEXT;

CREATE INDEX attachment_workspace_sha256_idx
    ON attachment (workspace_id, content_sha256)
    WHERE content_sha256 IS NOT NULL;
