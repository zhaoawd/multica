DROP INDEX IF EXISTS attachment_workspace_sha256_idx;
ALTER TABLE attachment
    DROP COLUMN IF EXISTS source,
    DROP COLUMN IF EXISTS content_sha256;
