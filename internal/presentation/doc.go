// Package presentation owns VibeShell's trusted presentation policy:
// the per-session MOTD generation pipeline, the VibeOS/VibeShell
// identity conventions (uname, os-release, shell identification), and
// the GNU/Hurd-style baseline seed description.
//
// Dependency direction: this package imports only the domain and the
// Go standard library. It performs no I/O, reads no clock, and never
// touches the real host: every fact it renders comes from validated
// inputs or from the simulated identity value, so the real container
// kernel, hostname, process list, resource usage, and environment can
// never appear in presented output.
//
// Two small consumer-side ports are defined here and implemented by
// adapters outside this package:
//
//   - MOTDPromptProvider supplies the administrator-controlled,
//     versioned MOTD generation prompt (backed by the prompts/ files
//     owned by the configuration/simulation workstream).
//   - MOTDGenerator renders a raw MOTD from a prompt and its factual
//     inputs (backed by an adapter over ports.ModelGateway).
//
// Prompt safety rule (PLAN 7.4): a presentation prompt is data. It
// can change only the wording of the welcome within the configured
// line budget. MOTDGenerationRequest deliberately carries no tool
// definitions, no scope policy, and no execution capability, so an
// edited prompt cannot expand hard tool/scope permissions or enable
// real execution. ValidatePrompt runs before a replacement prompt is
// accepted at reload time.
package presentation
