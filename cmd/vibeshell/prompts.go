package main

import (
	"os"
	"path"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/presentation"
)

// generationEmptyRenderings is the documented empty rendering for every
// generation-slot prompt variable, so a prompt file that omits a value still
// renders rather than failing closed. The built-in artifact ABI is appended
// separately, so an operator brief never has to restate the output shape.
var generationEmptyRenderings = map[string]string{
	"system_name":             "VibeOS",
	"command_name":            "the command",
	"command_argv":            "the command",
	"invocation_cwd":          "/",
	"requested_capabilities":  "text output",
	"parent_artifact_summary": "none",
}

// loadGenerationTemplate reads the configured generation prompt file, or returns
// "" when none is configured or readable.
func loadGenerationTemplate(cfg *config.Config, configDir string) string {
	if cfg.Prompts == nil || cfg.Prompts.AppGeneration == "" {
		return ""
	}
	file := cfg.Prompts.AppGeneration
	if !path.IsAbs(file) {
		file = path.Join(configDir, file)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	return string(raw)
}

// renderGenerationTemplate substitutes the generation slot's documented
// variables with the trusted invocation facts.
func renderGenerationTemplate(template string, req application.TurnRequest, name, systemName string) (string, error) {
	values := map[string]string{
		"system_name":             systemName,
		"command_name":            name,
		"command_argv":            req.Input.Command,
		"invocation_cwd":          req.Context.CWD.String(),
		"requested_capabilities":  "text output",
		"parent_artifact_summary": "none",
	}
	return presentation.RenderPrompt(template, values, generationEmptyRenderings)
}
