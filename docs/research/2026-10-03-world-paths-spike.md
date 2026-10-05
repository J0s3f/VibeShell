# World path resolution, scope overlays, and permission semantics spike

Date: 2026-10-03. Worktree: `.worktrees/world-paths`, branch `agent/world-paths`.
Module: `experiments/world-paths` (nested Go module, stdlib only;
`require j0s.at/vibeshell v0.0.0` + `replace j0s.at/vibeshell => ../..`,
imports `j0s.at/vibeshell/internal/domain` for `ValidPath`, `ParsePath`,
`Node/Mutation/ReadDependency/Revision`, `ScopePolicy`, `DomainError` codes).
Receipt: `experiments/world-paths/receipts/go-test-race.log` (`go test -race ./...` PASS).

## What was built (spike only, not application implementation)

- `resolve.go`: `ResolveInput(cwd, input, symlinks)` — absolute/relative
  resolution to `domain.ValidPath`, `.`/`..` normalization, `..` above root
  clamps at `/`, symlink expansion (absolute + relative targets) with
  loop/depth errors, strict component/input validation.
- `overlay.go`: three-layer `View{Baseline, Shared, User}` with
  user > shared > baseline precedence; tombstones hide lower layers without
  erasing; `DeleteUser` records `MutationTombstone`; `MaterializeUser`
  re-creates as NEW `MutationCreate` with fresh ID + `InitialRevision`.
- `perm.go`: `CheckUnix` (owner/group/other dispatch, simulated-root rule)
  and `Enforce` (trusted `ScopePolicy` gate first, then Unix bits).
- `store.go`: in-memory `SimStore` modelling the commit path — per-path
  version + parent-dir membership revision + absence read dependencies;
  `CommitCreate` rejects existing paths and stale dir revisions with
  `CodeDuplicateKey` / `CodeStaleRead` conflicts.

## PLAN rules exercised

- 5.2: deterministic resolution from session cwd + effective view; user
  overlay precedence; tombstone deletions; default user-associated writes;
  sharing-off rejects shared/other-user reads/writes at the tool boundary
  regardless of model requests; scope filters from trusted context.
- 5.3: versioned snapshot + staged change set; read/write dependency
  versions incl. directory membership + absence checks; atomic verify in one
  short transaction (simulated); conflict → refresh/rebase, never silent
  overwrite; no DB lock while generating (simulated by snapshot/commit
  split); `cd` is session-local (cwd is a pure function argument);
  symlinks/`..` stay inside the namespace, never a host path; deletion
  removes from view, not from history (lower layers retained).
- 5.5: unexplored paths resolve to staged creation; content authoritative
  only on commit; concurrent first access → one winner, losers re-read;
  deletion records are constraints; re-materialization is a new creation.
- 16.1 rows qualified: World exactness (overlay identity), Shared conflicts
  (one winner + stale-listing rejection), Scope (sharing-off denies
  cross-scope even at 0777 / as simulated root).

## Resolved rules (spike behavior)

1. Precedence: user (live or tombstone) > shared (live or tombstone) >
   baseline. Tombstone = hidden, lower layer bytes untouched.
2. Containment: `..` above `/` clamps; symlink targets (absolute, relative,
   `..`-containing, self/looping) re-enter the component stack; depth bound
   40; output is always `domain.ValidPath`, never a host path.
3. Validation: reject empty input, NUL, any C0/C1 control or DEL, invalid
   UTF-8, empty components (after `//` collapse), `.`/`..` as storable
   names, `MaxPathLen` (4096) / `MaxNameLen` (255) overruns.
4. Unicode: byte-exact keys; U+00E9 vs U+0065+U+0301 both validate and stay
   distinct — no silent collision, no normalization.
5. Unix: owner → group (egid or supplementary) → other; root (EUID 0)
   bypasses r/w, needs any x-bit for exec; root is simulated and never
   bypasses the scope-policy gate; baseline immutable.
6. Concurrency: first creator wins iff it presents absence + current dir rev;
   every other outcome is a `conflict` requiring rebase to winner's ID/rev.

## Decisions / ambiguities (flagged for main agent)

- `domain.ParsePath` currently rejects NUL/`.`/`..`/lengths but NOT other
  control bytes; the spike enforces a stricter control/DEL/UTF-8 boundary.
  Adopt the stricter rule into domain validation, or keep it at the adapter
  edge? Recommend the former.
- `Domain.ValidPath.Parent/Base/Join` exist but there is no domain-level
  `Resolve(cwd, input)` or symlink-aware resolver; the spike's resolver is
  the proposed semantic. Confirm clamp-at-root (vs erroring) for `..`
  above `/`.
- Spike `View` keys are path strings; the real store keys by
  `(namespace, parent, name)` with opaque IDs. Tombstone-as-`MutationTombstone`
  vs `MutationDelete` ownership needs B01 confirmation.
- Unicode: spike does NOT normalize (NFC/NFD distinct). Confirm: display
  may normalize, storage keys must not.
- Simulated-root exec rule (any-x-bit) follows POSIX; confirm desired for V1.
- `SimStore` is in-memory only; real atomicity/FTS/index behavior belongs
  to B01/B02, not proven here.

## Open questions

- Home-path encoding for unusual usernames (`/home/<user>` display vs key)?
- Shared-scope tombstone authorship/provenance fields?
- Exact dir-membership `ReadDependency` shape for nested parents (spike
  tracks immediate parent only)?

## Determinism / hygiene

No `time.Now`, no `math/rand`, no OS filesystem access in the module;
IDs/timestamps are test-injected constants. `go vet` clean.
