// Package config implements the operator-facing configuration boundary of
// VibeShell (PLAN 11) and the secure password file (PLAN 4.2).
//
// Responsibilities, in dependency order:
//
//   - Parse performs strict versioned JSON decoding: unknown fields,
//     duplicate object keys, wrong scalar kinds, nulls for non-null fields,
//     trailing content, missing required fields, and out-of-range or unknown
//     enum values are rejected with JSON-path diagnostics before any I/O.
//     A structural scan drives those checks from the Go type shape, so the
//     rules cannot drift from the document types.
//   - Semantic validation applies the PLAN 11 rules: duplicate
//     identifiers, tier/route/account/pool cross-references, provider,
//     product, and protocol combinations, prompt paths, and numeric bounds.
//     It performs no network calls: validation never makes an inference
//     request to decide whether a configuration is valid.
//   - Loader checks what needs the filesystem: referenced prompt files must
//     exist, be regular, and be readable, and account secret references
//     resolve from mounted files (only inside the allowed secret
//     directories) or from container environment variables. Resolved values
//     live only in memory as Secret, whose string forms are redacted.
//   - Store publishes a fully validated Snapshot atomically. A turn pins one
//     snapshot for its whole lifetime; a reload that fails validation keeps
//     the previous snapshot and its version.
//   - PasswordFile maintains the versioned Argon2id password file with
//     bounded hash parameters, timing-safe comparison, and dummy
//     verification for unknown or disabled usernames. Passwords are read
//     from an io.Reader (stdin/TTY), never from a command-line argument.
//
// The document types mirror schemas/configuration.schema.json for the
// baseline groups. Groups beyond that baseline cover the remaining PLAN 11
// groups and are pending main-agent ratification of the shared schema; see
// docs/configuration.md for the field reference, the hot-reload boundary,
// and the schema delta.
package config
