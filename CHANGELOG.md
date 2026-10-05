# Changelog

Notable changes per release. The published container image
(`ghcr.io/j0s3f/vibeshell`) is built from each `v*` tag for `linux/amd64` and
`linux/arm64`.

## v0.1.0

First published release.

- **Simulated-world shell.** `ls`, `cat`, `cd`, `pwd`, `mkdir`, `touch`, `rm`,
  `echo`, and `env` run against a durable, per-user simulated filesystem; a path
  that does not exist is materialized on demand and then persists.
- **Generated applications.** A novel command is written by a language model,
  staging-validated, activated, and executed only inside the bounded
  QuickJS/Wasm sandbox.
- **Line-based interactive applications.** A generated app can stay foreground
  across lines with its own prompt and carried-forward state, and return to the
  shell on exit or end of input.
- **Model routing.** Direct OpenCode HTTP (Go) and OpenCode CLI providers behind
  one gateway, with per-purpose selection, session affinity, health, and
  failover.
- **Persistence.** SQLite with WAL, atomic content, optimistic revision checks,
  live backup/restore, crash recovery, and redaction.
- **Terminal.** An SSH listener with a line editor (history, cursor motion,
  Home/End, Ctrl-A/E/U/K/W, bracketed paste).
- **Container image.** Multi-platform runtime image with a mounted configuration
  and a persistent state volume.
