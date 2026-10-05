# VibeShell example configuration (PLAN 11)

`vibeshell.conf.json` is the documented example configuration
for the strict configuration loader qualified in
`experiments/config-validation`. It loads as-is when the
secrets directory and environment below are provided.

## What the example demonstrates

- **Tier 1 `named-free`**: explicit named free routes, in
  the operator's chosen order.
- **Tier 2 `free-discovery`**: suitable-free discovery
  (`auto_free: true`) with the pattern `opencode/-free`,
  which expands to every catalogue route of the `opencode`
  product whose ID ends in `-free`, in catalogue order.
- **Tier 3 `go-native`**: Go-native routes
  (`opencode-go/...`).
- **Tier 4 `cheap-payg`**: explicitly allowed cheap
  pay-as-you-go routes, restricted to the `payg-accounts`
  account pool.

The tier ordering is an example, not an enforced
preference (PLAN 11): administrators can reorder, omit, or
mix tiers and eligible accounts. Model IDs are examples
verified against the catalogue at implementation time; no
entry claims permanent availability of any model.

## Secret references

Secrets travel as references, never values:

- `accounts/opencode-primary.key` resolves to
  `<secrets-dir>/accounts/opencode-primary.key`, where
  `<secrets-dir>` is the loader's allowed secrets
  directory.
- `VIBESHELL_ZEN_API_KEY_FAKE` resolves from the process
  environment.

The resolved values never appear in errors, logs, or
formatted output.

## Prompt files

`prompts/*.md` are loaded relative to this directory. A
missing prompt file fails the load.

## Groups not in contract version 1

`providers`, `prompts`, `discovery`, `inference`, and
`operations` are PLAN 11 groups that the version 1 schema
(`schemas/configuration.schema.json`) does not carry yet;
they are optional in the loader and documented as schema
version 2 candidates in the qualification report
(`docs/research/2026-10-03-config-validation-spike.md`).
