package presentation

import (
	"strings"
	"testing"
)

func TestValidatePromptVersion(t *testing.T) {
	if err := ValidatePromptVersion(DefaultPromptVersion); err != nil {
		t.Fatalf("default version rejected: %v", err)
	}
	for _, bad := range []string{"", "MOTD-V1", "has space", "semi;colon", strings.Repeat("v", MaxPromptVersionLen+1)} {
		if err := ValidatePromptVersion(bad); err == nil {
			t.Fatalf("version %q accepted, want rejection", bad)
		}
	}
}

func TestValidatePrompt(t *testing.T) {
	good := MOTDPrompt{Version: "motd-v1", Text: "Write a short welcome."}
	if err := ValidatePrompt(good); err != nil {
		t.Fatalf("good prompt rejected: %v", err)
	}
	bad := []MOTDPrompt{
		{Version: "", Text: "hi"},
		{Version: "motd-v1", Text: "   "},
		{Version: "motd-v1", Text: strings.Repeat("x", MaxPromptTextBytes+1)},
		{Version: "motd-v1", Text: "welcome\x00guest"},
		{Version: "motd-v1", Text: "welcome\x1b[1mguest"},
		{Version: "Bad Version!", Text: "hi"},
	}
	for i, p := range bad {
		if err := ValidatePrompt(p); err == nil {
			t.Fatalf("case %d accepted, want rejection: %+v", i, p)
		}
	}
}

func TestRenderPrompt(t *testing.T) {
	values := map[string]string{
		"system_name":        "VibeOS",
		"displayed_username": "alice",
		"session_elapsed":    "", // empty falls back to the documented rendering
	}
	empty := map[string]string{
		"session_elapsed": "less than a minute",
		"prompt_version":  "unversioned",
	}
	out, err := RenderPrompt(
		"Hello {{displayed_username}} on {{system_name}}; {{session_elapsed}}; {{prompt_version}}",
		values, empty)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	if strings.Contains(out, "{{") {
		t.Fatalf("rendered output still has a placeholder: %q", out)
	}
	if want := "Hello alice on VibeOS; less than a minute; unversioned"; out != want {
		t.Fatalf("rendered = %q, want %q", out, want)
	}
	if _, err := RenderPrompt("{{undocumented_variable}}", nil, empty); err == nil {
		t.Fatal("undocumented placeholder accepted, want rejection")
	}
}

func TestRenderMOTDPrompt(t *testing.T) {
	out, err := RenderMOTDPrompt("{{displayed_username}}/{{prompt_version}}/{{session_elapsed}}", map[string]string{
		"displayed_username": "alice",
		"prompt_version":     DefaultPromptVersion,
	})
	if err != nil {
		t.Fatalf("RenderMOTDPrompt: %v", err)
	}
	if want := "alice/" + DefaultPromptVersion + "/less than a minute"; out != want {
		t.Fatalf("rendered = %q, want %q", out, want)
	}
	if _, err := RenderMOTDPrompt("{{session_cwd}}", nil); err == nil {
		t.Fatal("a non-MOTD variable was accepted by the MOTD renderer")
	}
}

func TestMOTDConfigValidation(t *testing.T) {
	cfg := DefaultMOTDConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
	badLine := cfg
	badLine.LineBudget = 0
	if err := badLine.Validate(); err == nil {
		t.Fatal("zero line budget accepted")
	}
	badBytes := cfg
	badBytes.MaxBytes = -1
	if err := badBytes.Validate(); err == nil {
		t.Fatal("negative byte bound accepted")
	}
	badVer := cfg
	badVer.PromptVersion = ""
	if err := badVer.Validate(); err == nil {
		t.Fatal("empty prompt version accepted")
	}
}
