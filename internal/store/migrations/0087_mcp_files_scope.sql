-- An MCP token reads Drive without Apps/ (each app's own data, credential
-- stores among it) and without dotfiles, unless the operator minted it with
-- this right. Existing tokens get the narrow default: the broad read was
-- never something the Settings page asked about.
ALTER TABLE mcp_tokens ADD COLUMN can_read_all_files INTEGER NOT NULL DEFAULT 0;
