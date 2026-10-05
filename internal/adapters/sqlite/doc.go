// Package sqlite holds VibeShell's SQLite storage adapters.
//
// B01 (world storage) owns the migration framework in migrate.go, the
// migration numbering, and every world*.go file. B02 (research/event
// storage) owns events*.go and submits schema needs to B01 instead of
// numbering migrations independently.
package sqlite
