package presentation

import (
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

func TestDefaultBaselineSeedIsValid(t *testing.T) {
	seed := DefaultBaselineSeed()
	if err := seed.Validate(); err != nil {
		t.Fatalf("default seed invalid: %v", err)
	}
	if seed.Version != BaselineSeedVersion {
		t.Fatalf("version = %q, want %q", seed.Version, BaselineSeedVersion)
	}
	paths := map[string]BaselineEntry{}
	for _, e := range seed.Directories {
		paths[string(e.Path)] = e
	}
	for _, want := range []string{"/", "/home", "/root", "/tmp", "/etc", "/usr", "/bin", "/dev", "/proc"} {
		if _, ok := paths[want]; !ok {
			t.Fatalf("seed missing %s", want)
		}
	}
	if paths["/tmp"].Mode != 0o1777 {
		t.Fatalf("/tmp mode = %#o, want 0o1777", paths["/tmp"].Mode)
	}
	if paths["/proc"].Mode != 0o555 {
		t.Fatalf("/proc mode = %#o, want 0o555", paths["/proc"].Mode)
	}
	for _, e := range seed.Directories {
		if e.Kind != domain.NodeKindDir {
			t.Fatalf("%s kind = %v, want dir", e.Path, e.Kind)
		}
		if e.UID != RootUID || e.GID != RootGID {
			t.Fatalf("%s owner = %d/%d, want root", e.Path, e.UID, e.GID)
		}
		if e.Purpose == "" {
			t.Fatalf("%s has no purpose", e.Path)
		}
	}
}

func TestBaselineSeedCarriesNoContents(t *testing.T) {
	// The seed is identity and structure only: file and symlink
	// contents are materialized on demand, never seeded here.
	seed := DefaultBaselineSeed()
	for _, e := range seed.Directories {
		if e.Kind != domain.NodeKindDir {
			t.Fatalf("non-directory entry %s would imply seeded contents", e.Path)
		}
	}
}

func TestBaselineSeedValidation(t *testing.T) {
	dup := DefaultBaselineSeed()
	dup.Directories = append(dup.Directories, dup.Directories[0])
	if err := dup.Validate(); err == nil {
		t.Fatal("duplicate path accepted")
	}
	unsorted := DefaultBaselineSeed()
	unsorted.Directories[0], unsorted.Directories[1] = unsorted.Directories[1], unsorted.Directories[0]
	if err := unsorted.Validate(); err == nil {
		t.Fatal("unsorted directories accepted")
	}
	empty := DefaultBaselineSeed()
	empty.Version = ""
	if err := empty.Validate(); err == nil {
		t.Fatal("empty version accepted")
	}
}

func TestSanitizeMOTD(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"fences removed", "```\nhello\n```", "hello"},
		{"tilde fences removed", "~~~\nhi\n~~~", "hi"},
		{"ansi removed", "\x1b[31mred\x1b[0m plain", "red plain"},
		{"osc removed", "a\x1b]0;title\x07b", "ab"},
		{"controls dropped", "a\x00b\x01c", "abc"},
		{"blank edges trimmed", "\n\nhello\n\n", "hello"},
		{"crlf normalized", "a\r\nb", "a\nb"},
		{"utf8 preserved", "héllo wörld ✓", "héllo wörld ✓"},
		{"tabs kept", "a\tb", "a\tb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeMOTD(tc.raw); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	if got := SanitizeMOTD("```\n```"); got != "" {
		t.Fatalf("fence-only input = %q, want empty", got)
	}
	if got := strings.Count("a\nb\nc\nd", "\n") + 1; got != 4 {
		t.Fatal("test sanity check failed")
	}
}
