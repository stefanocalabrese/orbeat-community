-- +goose Up
-- Global-scope rules. A rule is PROJECT-scoped by default, landing in each
-- registered project's AGENTS.md, which is what every rule shipped before this
-- does. A GLOBAL rule instead lands in the user-level instruction files that
-- every project inherits, for instructions that are about the developer rather
-- than about a repository ("always ask before force-pushing").
--
-- NULL means project, rather than a NOT NULL DEFAULT 'project', so an existing
-- row is untouched and the absence of a value keeps meaning what it meant. The
-- CHECK still constrains the values that CAN appear, so the column cannot
-- become a free-text field the way mcp_server.status did before 00009.
ALTER TABLE artifact
    ADD COLUMN rule_scope text
        CHECK (rule_scope IS NULL OR (rule_scope IN ('project', 'global') AND type = 'rule')),
    ADD COLUMN approved_rule_scope text
        CHECK (approved_rule_scope IS NULL OR
               (approved_rule_scope IN ('project', 'global') AND approved_type = 'rule'));

-- Snapshotted for the same reason as target_tags (00024) and visibility
-- (00016): flipping a rule from project to global changes WHO SEES IT, from the
-- developers who registered a matching project to every developer who syncs at
-- all. That is a governed change, so it waits for an approval.
ALTER TABLE artifact_revision
    ADD COLUMN rule_scope text;

-- +goose Down
ALTER TABLE artifact_revision DROP COLUMN rule_scope;
ALTER TABLE artifact DROP COLUMN rule_scope, DROP COLUMN approved_rule_scope;
