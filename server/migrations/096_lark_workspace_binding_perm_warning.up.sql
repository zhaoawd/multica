-- Throttle the §14.1.3 permission-warning bot reply.
--
-- When @bot 创建任务 tries to download a thread attachment and the
-- Lark API rejects with "missing scope im:resource", the bot replies
-- once into the thread asking an admin to grant the scope. Without
-- throttling the bot would post that line on every single message
-- with an attachment — the design pins this to "once" per binding,
-- using this column as the rate-limit cursor.
--
-- NULL means "no warning has been emitted for this workspace yet".
-- The notify path updates it via SET last_perm_warning_at = now()
-- in the same transaction that posts the bot reply.
ALTER TABLE lark_workspace_binding
    ADD COLUMN last_perm_warning_at TIMESTAMPTZ;
