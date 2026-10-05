# Contract fixtures

Representative fixtures for the shared contracts of PLAN 14. `manifest.json` is
the index: it maps each contract to its shipped schema (or says that no schema
has shipped), to the representative Go structure a fixture must decode into, and
to the edge cases the fixtures cover.

Checks live in `../contract_test.go` and run as part of `go test -race ./...`:
every registered fixture must exist, be valid JSON, decode strictly into its
representative structure, and round-trip without loss.

## What is here

| Contract | Schema | Fixtures |
| --- | --- | --- |
| model request / result | shipped | `model/` |
| world snapshot / change set | shipped | `world/` |
| event envelope | shipped | `session/`, `app/sandbox-exception-run.json` |
| app artifact / event / result | shipped | `app/` |
| route policy / health | shipped | `routing/` |
| session input / output | proposed | `proposed/session-input-output.json` |
| agent tool | proposed | `proposed/agent-tool-unknown-command.json` |
| retrieval | proposed | `proposed/retrieval-incomplete-delivery.json` |
| configuration | proposed | `proposed/config-prompts-group.json` |

## Relationship to `schemas/fixtures/`

A02 already shipped eleven minimal fragments, one per PLAN 14 edge case. They
are referenced, not copied. What those fragments do not carry is a *complete
contract instance*: a bare result with no request, a bare error with no
exchange, a bare payload with no envelope, a run payload that no schema accepts
as written. Each fixture here is the complete form, so a test can decode it into
the same representative structure an adapter will use and round-trip it.

For every edge case, `manifest.json` records both the owned fixture and the
existing fragment, plus what the owned one adds. Summary:

| Edge case | Existing fragment | Added here |
| --- | --- | --- |
| empty output | bare result | full request/result exchange, zero-token completion |
| Unicode | input fragment | non-ASCII, emoji and non-ASCII filename through an exchange |
| binary content reference | bare content reference | reference used by a tool call and by a committed write |
| unknown command | one input event | materialization and candidate calls, then the activated artifact |
| new folder | single directory create | both parents plus the absence/membership revisions |
| shared-scope denial | bare domain error | persisted typed tool error, nothing written |
| concurrent conflict | expected/actual revisions | typed failure plus the re-staged change set |
| quota error | bare error envelope | exchange result plus route health cooldown |
| cancelled generation | bare error envelope | cancelled result with the partial content already streamed |
| sandbox exception | bare run payload | persisted run envelope, no state written, view retained |
| incomplete transcript delivery | retrieval fragment | truncated frame by content reference, page with next cursor |

## Proposed fixtures

PLAN 14 makes the main agent the owner of the shared contract baseline; other
agents propose changes through it. Session input/output, agent tool and
retrieval have no shipped schema today, and `configuration.schema.json` version
1 has no `prompts` group although PLAN 11 requires one. The four fixtures under
`proposed/` therefore show a *candidate* shape. They are clearly separated, they
are not frozen contracts, and adopting them means the main agent publishes a
schema and moves the fixture.

`proposed/config-prompts-group.json` is deliberately invalid against
`configuration.schema.json` version 1 for exactly that reason: it is the
proposal that adds the `prompts` group selecting the files in `prompts/`.

## Conventions used by the fixtures

* Identities use the shipped pattern `^[a-z]{2,3}_[0-9A-HJKMNP-TV-Z]{26}$` and a
  shared synthetic tail, so several fixtures read as one continuing session.
* Tool names (`world.stage`, `world.materialize`, `apps.candidate`) are
  placeholders. PLAN 7.2 defines tool *capabilities*, not names, and no
  agent-tool schema has shipped.
* The generated JavaScript source uses an `onEvent(state, event)` entrypoint.
  The app-abi schema fixes the manifest and event/result shapes, not a source
  convention.
* Nothing in this directory contains a credential, a password, a hash of one, or
  provider diagnostics.