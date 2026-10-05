package sqlite

// Schema step 3 makes the generated-application store and the route/account
// health state durable (PLAN 5.6, 9.2, 9.3, 10.2). It is one migration rather
// than three because migration numbering has a single owner and a version must
// never be split across registrations; each fragment lives next to the adapter
// that owns those rows, and this file composes and registers them.

// appRegistryV3Schema holds the generated-application tables. Artifacts are
// written once and are only ever stamped with their first activation timestamp,
// so a registered version can never be overwritten by partially streamed or
// invalid source (PLAN 5.6).
const appRegistryV3Schema = `
CREATE TABLE apps (
	app_id TEXT PRIMARY KEY,
	owner TEXT NOT NULL,
	scope TEXT NOT NULL,
	current_version TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);

CREATE TABLE app_versions (
	version_id TEXT PRIMARY KEY,
	app_id TEXT NOT NULL REFERENCES apps(app_id) ON DELETE RESTRICT,
	parent_version TEXT NOT NULL DEFAULT '',
	manifest TEXT NOT NULL,
	source TEXT NOT NULL,
	source_hash TEXT NOT NULL,
	owner TEXT NOT NULL,
	scope TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	provenance TEXT NOT NULL,
	validation TEXT NOT NULL,
	activated_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_app_versions_app ON app_versions(app_id, created_at);

-- Durable app state, one row per (scope, owner, app). The version that
-- produced the state travels with it, because a later version needs a verified
-- additive migration before it may interpret that state (PLAN 5.6).
CREATE TABLE app_state (
	state_scope TEXT NOT NULL,
	owner TEXT NOT NULL DEFAULT '',
	app_id TEXT NOT NULL REFERENCES apps(app_id) ON DELETE RESTRICT,
	version_id TEXT NOT NULL,
	state TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY(state_scope, owner, app_id)
);

-- Session version pins: a running session keeps the version it resolved plus
-- its session-scoped state portion, even after the current pointer moves.
CREATE TABLE app_session_pins (
	session_id TEXT NOT NULL,
	app_id TEXT NOT NULL REFERENCES apps(app_id) ON DELETE RESTRICT,
	version_id TEXT NOT NULL,
	session_state TEXT NOT NULL,
	pinned_at INTEGER NOT NULL,
	PRIMARY KEY(session_id, app_id)
);

-- The activation and rollback log kept for research: every pointer move, with
-- the version it came from and, for a rollback, the operator's reason.
CREATE TABLE app_activations (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	app_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	from_version TEXT NOT NULL DEFAULT '',
	to_version TEXT NOT NULL,
	at INTEGER NOT NULL,
	reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_app_activations_app ON app_activations(app_id, seq);
`

const migrationV3 = appRegistryV3Schema + routingHealthV3Schema + spendingV3Schema

func init() {
	RegisterMigration(Migration{Version: 3, Name: "apps-routing-durable-state", SQL: migrationV3})
}
