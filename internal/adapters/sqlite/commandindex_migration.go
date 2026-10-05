package sqlite

// Schema step 4 adds the durable command-name index (audit finding B1): the
// shell's app-to-command routing table used to live only in the generation
// engine's memory, so an app accepted by an earlier process was unreachable
// by name after a restart. One row per visible command name; the name is the
// key because a later app legitimately takes a name over (last write wins),
// and the referenced app must exist because names are recorded from a
// registered manifest.
const appCommandIndexV4Schema = `
CREATE TABLE app_commands (
	command TEXT PRIMARY KEY,
	app_id TEXT NOT NULL REFERENCES apps(app_id) ON DELETE RESTRICT
);
CREATE INDEX idx_app_commands_app ON app_commands(app_id);
`

func init() {
	RegisterMigration(Migration{Version: 4, Name: "app-command-index", SQL: appCommandIndexV4Schema})
}
