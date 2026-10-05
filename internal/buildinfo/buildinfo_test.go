package buildinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCurrentReportsThePublicIdentity(t *testing.T) {
	info := Current()

	if info.System != "VibeOS" {
		t.Errorf("System = %q, want %q", info.System, "VibeOS")
	}
	if info.Shell != "VibeShell" {
		t.Errorf("Shell = %q, want %q", info.Shell, "VibeShell")
	}
	if info.Version != DefaultVersion {
		t.Errorf("Version = %q, want %q for an unstamped build", info.Version, DefaultVersion)
	}
	if isKnownRevision(info.Revision) {
		t.Errorf("Revision = %q, want an unstamped build to report no revision", info.Revision)
	}
}

func TestInfoStringNamesVersionAndRevision(t *testing.T) {
	tests := []struct {
		name string
		info Info
		want string
	}{
		{
			name: "unstamped build",
			info: Info{System: "VibeOS", Shell: "VibeShell", Version: DefaultVersion, Revision: unknownRevision},
			want: "VibeShell on VibeOS version dev",
		},
		{
			name: "stamped release build",
			info: Info{System: "VibeOS", Shell: "VibeShell", Version: "0.1.0", Revision: "1a2b3c4d5e6f"},
			want: "VibeShell on VibeOS version 0.1.0 (1a2b3c4d5e6f)",
		},
		{
			name: "dirty development build",
			info: Info{System: "VibeOS", Shell: "VibeShell", Version: DefaultVersion, Revision: "1a2b3c4d5e6f-dirty"},
			want: "VibeShell on VibeOS version dev (1a2b3c4d5e6f-dirty)",
		},
		{
			name: "missing revision",
			info: Info{System: "VibeOS", Shell: "VibeShell", Version: DefaultVersion, Revision: ""},
			want: "VibeShell on VibeOS version dev",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.info.String(); got != test.want {
				t.Errorf("String() = %q, want %q", got, test.want)
			}
		})
	}
}

// The linker silently ignores a stamp whose symbol path does not exist, so the
// only place that notices is a released artifact. This test fails as soon as
// the Containerfile drifts from the variables this package owns.
func TestContainerfileStampsTheBuildInfoVariables(t *testing.T) {
	const containerfile = "../../containers/Containerfile"

	contents, err := os.ReadFile(filepath.Clean(containerfile))
	if err != nil {
		t.Fatalf("reading %s: %v. Run tests from the repository checkout.", containerfile, err)
	}

	definition := string(contents)
	for _, variable := range []string{"version", "revision"} {
		symbol := "j0s.at/vibeshell/internal/buildinfo." + variable + "="
		if !strings.Contains(definition, "-X "+symbol) {
			t.Errorf("%s does not stamp -X %s; released binaries would report an unstamped build", containerfile, symbol)
		}
	}
}
