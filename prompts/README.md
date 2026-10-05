# Default prompts

Versioned, plain-text prompts for VibeOS/VibeShell presentation and agent
behaviour. They are data, not code: an administrator edits or replaces them,
and `tests/contract` checks the shipped set for placeholder and rendering
drift.

| Path | Purpose | Plan |
| --- | --- | --- |
| `v1/motd.txt` | One fresh login message per accepted session | 7.4 |
| `v1/shell-behavior.txt` | Baseline simulated-shell behaviour | 5.4, 5.5, 6.1, 7.4 |
| `v1/app-generation.txt` | Artifact for a program the world has not implemented | 5.5, 5.6 |
| `v1/app-extension.txt` | Candidate version for unsupported options and modes | 5.6, 6.3 |
| `v1/world-materialization.txt` | Staged change set for unexplored paths | 5.4, 5.5 |
| `v1/summary-repair.txt` | Durable summaries and failed-turn repair notes | 7.3, 9.2 |
| `manifest.json` | Machine-readable inventory, variables, constraints | 11 |

## Selection and versioning

`manifest.json` describes the prompt set (`prompt_set: "v1"`) and every prompt
slot. The configuration `prompts` group (PLAN 11) holds the path of each slot;
a path is administrator-controlled and validated before reload. Version
directories let a new set coexist with an old one, so a rollout can point one
slot at a new file and roll it back by pointing it back.

The selected prompt version is recorded with the prompt and configuration
snapshot of each turn, in the event envelope's `provenance.prompt_version` and
`provenance.config_version`. A turn pins the snapshot it started with; the next
turn or session uses the updated permitted version.

## Placeholder variables

Syntax: `{{variable_name}}`, substituted by the trusted renderer before the
prompt is sent. The model never receives the raw file.

* Every placeholder in a prompt file is listed in that prompt's `variables`
  array in `manifest.json`; `tests/contract` fails on an undocumented one.
* Every documented variable appears in the file it is documented for, so the
  documentation cannot drift into describing a variable nobody renders.
* Every documented variable has an `empty_rendering`. A value-less variable
  renders as that text, so a prompt never reaches the model with an unresolved
  `{{...}}`.
* Variable values are session facts only. Passwords, password hashes and API
  keys are never prompt variables, and a session secret must not reach a
  variable by accident.

Shared vocabulary:

| Variable | Meaning | Empty rendering |
| --- | --- | --- |
| `system_name` | Presented OS name (`VibeOS`) | `VibeOS` |
| `shell_name` | Presented shell name (`VibeShell`) | `VibeShell` |
| `simulated_hostname` | Simulated host name, never the real container host name | `vibeshell` |
| `prompt_version` | Label of the selected prompt file, recorded as provenance | `unversioned` |
| `displayed_username` | User name shown at login | `user` |
| `sharing_mode` | Enabled sharing mode as a short token | `sharing off` |
| `recording_status` | Permanent research-recording status as a short token | `recording off` |

`manifest.json` is the authoritative per-prompt list; the table above only
summarises the variables several prompts share.

## Hard rendering constraints

These hold for the shipped files and for any edited replacement, and they are
enforced by the runtime rather than by the model obeying the text:

1. No Markdown fences. Shipped prompt files contain no backtick character at
   all, which `tests/contract` checks.
2. No model commentary. A prompt asks for the deliverable only: no reasoning,
   no self-description, no "here is your ...".
3. No terminal control sequences, colour codes or cursor movement. Plain text
   lines only. The trusted renderer owns the shell prompt, the screen state and
   every control character.
4. No real host, kernel, container, process, resource or network facts may be
   requested, reported or invented. `uname`, OS-release views and the MOTD
   present VibeOS; shell identification presents VibeShell.
5. No credentials in any variable or prompt output.

## An edited prompt changes presentation, never permissions

A prompt file is not trusted configuration for authority. Editing one may
change wording, tone, structure and the facts the model is told; it may never

* widen the tool set a turn may call,
* widen session, user, shared or baseline scope beyond the accepted session
  state and the current sharing policy,
* bypass staged world changes, revision checks, conflict retries or commit
  verification,
* grant a generated application a simulated capability the guest does not
  provide, or make generated JavaScript leave the bounded sandbox,
* enable real execution of a user command, generated source, or anything else,
  on the host, in a container, or in any unrestricted runtime,
* suppress the simulation or permanent-recording disclosure, or un-record
  events that were already recorded.

Prompt validation before reload checks that every placeholder is documented and
that the file is valid UTF-8 plain text. The hard boundaries above are enforced
by the runtime, so an edited prompt that claims otherwise is ignored: the claim
has no effect, and the turn proceeds under the permissions the session already
had.

## Relationship to PLAN 7.4

The shipped `v1/motd.txt` carries the PLAN 7.4 draft as its instruction text,
with the factual inputs of that section rendered as documented variables
(`displayed_username`, `session_started_at`, `session_elapsed`, `sharing_mode`,
`recording_status`). The wording is configurable, and the trusted renderer
appends the prompt afterwards.