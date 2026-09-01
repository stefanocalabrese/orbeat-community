-- +goose Up
-- The artifact deployment registry (docs/specs/2026-08-22-orbeat-artifact-
-- deployment-registry-design.md sec 6.1): which artifact, at which revision,
-- is actually ON a developer's machine. orbeat could already answer who is
-- ENTITLED to an artifact and it served the bytes, but nothing recorded that
-- any of it landed, so "how many people are on the fix" had no answer.
--
-- CURRENT STATE, not an event log. A report from one install REPLACES that
-- install's rows (store.ReplaceDeployments), so this table is bounded by
-- users x installs x entitled artifacts and does not grow with time. The one
-- axis that does grow is abandoned installs: a reimaged laptop leaves rows
-- keyed on an install_id that no longer exists anywhere and that nothing will
-- ever replace, which is what the reported_at index and the retention prune
-- below exist for.
--
-- The grain is per user per install, and it is deliberately not coarser: an
-- aggregate over users alone cannot say how many machines are behind, and
-- COUNT(DISTINCT user_id) needs the per-install rows to exist to be computed
-- at all (spec sec 4.1, sec 8.4).
--
-- Migrations are shared across editions, so this table exists in a Community
-- database too. Nothing there writes to it: the store functions named below
-- live in internal/store/artifact_deployment.ee.go and are dropped from a
-- generated Community tree by filename, and the registry is Enterprise only
-- (spec sec 11). On Community the table stays empty, which costs one empty
-- relation and keeps the two schemas identical.
CREATE TABLE artifact_deployment (
    tenant_id   uuid NOT NULL REFERENCES tenant(id) ON DELETE CASCADE,
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    install_id  uuid NOT NULL,
    artifact_id uuid NOT NULL REFERENCES artifact(id) ON DELETE CASCADE,
    revision    int  NOT NULL CONSTRAINT artifact_deployment_revision_positive CHECK (revision >= 1),
    reported_at timestamptz NOT NULL DEFAULT now(),

    -- No surrogate id. The natural key IS the identity of a deployment: one
    -- artifact, on one install, belonging to one user. A uuid primary key
    -- would add a column nothing ever selects by and would let the same
    -- triple appear twice, which is the one thing replace-on-report exists to
    -- prevent.
    PRIMARY KEY (user_id, install_id, artifact_id)
);

-- install_id carries NO foreign key, deliberately. It names no row anywhere:
-- it is an opaque uuid the client generates once and keeps in
-- ~/.config/orbeat/install.json, and orbeat holds no install table to point
-- it at (spec sec 4.2). It exists to GROUP rows, never to look anything up.
--
-- row_version is deliberately absent too. 00013's optimistic-concurrency
-- trigger guards an admin edit against a second admin; a report is
-- last-writer-wins by construction, because the newest report from an install
-- IS the truth about that install, and there is no second writer to lose an
-- update to.

-- The artifact axis: every admin read is "how is THIS artifact deployed",
-- tenant-scoped like every other read in the product.
CREATE INDEX artifact_deployment_artifact_idx ON artifact_deployment (tenant_id, artifact_id);

-- The retention axis. The prune (store.PruneDeploymentsOlderThan) filters
-- reported_at < cutoff ACROSS tenants, the same shape 00011's
-- audit_event_ts_idx serves for the audit prune, so a tenant-leading index
-- could not serve it.
CREATE INDEX artifact_deployment_reported_idx ON artifact_deployment (reported_at);

-- OPERATOR NOTE, and it changes the meaning of a route that already exists.
-- artifact_deployment.user_id is the FIRST foreign key in this schema
-- pointing at users.id. DELETE /v1/admin/users/{id} (internal/api/api.go,
-- handleDeleteUser) therefore stops being a single-row delete: it now also
-- destroys every row recording what that person's machines had.
--
-- That cascade is the design, not a side effect. It is the erasure path for
-- the deployment records about one named individual (spec sec 8.3), and
-- leaving those rows behind would be a per-person data remnant whose owner no
-- longer exists. store.DeletedUser's doc comment, which described a user row
-- as a leaf, is corrected in the same commit as this migration.

-- +goose Down
DROP TABLE artifact_deployment;
