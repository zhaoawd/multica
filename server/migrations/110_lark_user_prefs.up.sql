-- Per-user Lark notification preferences (§6.1 personal-mode signals).
-- Stored as JSONB so new preference fields don't require a migration.
-- Default matches DefaultLarkUserPref(): assigned_dm + agent_clarification_dm ON.
ALTER TABLE lark_user_link
    ADD COLUMN prefs JSONB NOT NULL DEFAULT '{"assigned_dm":true,"agent_clarification_dm":true}';
