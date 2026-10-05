package worldpaths

import (
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

func mustPath(t *testing.T, s string) domain.ValidPath {
	t.Helper()
	p, err := domain.ParsePath(s)
	if err != nil {
		t.Fatalf("ParsePath(%q): %v", s, err)
	}
	return p
}

func TestResolveAbsoluteAndRelative(t *testing.T) {
	cwd := mustPath(t, "/home/alice")
	got, err := ResolveInput(cwd, "/etc/motd", nil)
	if err != nil || got != domain.ValidPath("/etc/motd") {
		t.Fatalf("absolute: got %q err %v", got, err)
	}
	got, err = ResolveInput(cwd, "notes.txt", nil)
	if err != nil || got != domain.ValidPath("/home/alice/notes.txt") {
		t.Fatalf("relative: got %q err %v", got, err)
	}
	got, err = ResolveInput(cwd, "./sub/../notes.txt", nil)
	if err != nil || got != domain.ValidPath("/home/alice/notes.txt") {
		t.Fatalf("dot normalization: got %q err %v", got, err)
	}
	got, err = ResolveInput(mustPath(t, "/a/b"), "../../c", nil)
	if err != nil || got != domain.ValidPath("/c") {
		t.Fatalf("dotdot: got %q err %v", got, err)
	}
}

func TestResolveNeverEscapesRoot(t *testing.T) {
	cwd := mustPath(t, "/")
	got, err := ResolveInput(cwd, "../../..", nil)
	if err != nil || got != domain.ValidPath("/") {
		t.Fatalf("escape above root: got %q err %v", got, err)
	}
	got, err = ResolveInput(mustPath(t, "/a"), "../../../../etc", nil)
	if err != nil || got != domain.ValidPath("/etc") {
		t.Fatalf("deep escape: got %q err %v", got, err)
	}
	// Symlink pointing at ".." from root stays inside.
	got, err = ResolveInput(mustPath(t, "/a"), "../..", map[string]string{"/a": ".."})
	if err != nil || got != domain.ValidPath("/") {
		t.Fatalf("symlink escape: got %q err %v", got, err)
	}
	// Absolute symlink target is still a simulated path.
	got, err = ResolveInput(mustPath(t, "/a"), "link/x", map[string]string{"/a/link": "/etc"})
	if err != nil || got != domain.ValidPath("/etc/x") {
		t.Fatalf("abs symlink: got %q err %v", got, err)
	}
	// Relative symlink target resolves against its own directory.
	got, err = ResolveInput(mustPath(t, "/"), "/a/link/y", map[string]string{"/a/link": "sub"})
	if err != nil || got != domain.ValidPath("/a/sub/y") {
		t.Fatalf("rel symlink: got %q err %v", got, err)
	}
}

func TestResolveSymlinkLoopAndDepth(t *testing.T) {
	cwd := mustPath(t, "/")
	if _, err := ResolveInput(cwd, "/a", map[string]string{"/a": "/b", "/b": "/a"}); err == nil {
		t.Fatal("symlink loop succeeded, want error")
	}
	if _, err := ResolveInput(cwd, "/self", map[string]string{"/self": "/self"}); err == nil {
		t.Fatal("self symlink succeeded, want error")
	}
}

func TestRejectBadInputs(t *testing.T) {
	cwd := mustPath(t, "/home/alice")
	bad := []string{
		"",
		"/a\x00b",
		"/a\x01b",
		"/a\x7fb",
		"ok\x1b",
		"/" + strings.Repeat("x", 300),
		strings.Repeat("/seg", 1100), // over MaxPathLen
		"/a//b",                      // empty component collapses, but "//" alone must not create host meaning; accepted as single slash
	}
	for _, in := range bad[:7] {
		if _, err := ResolveInput(cwd, in, nil); err == nil {
			t.Errorf("ResolveInput(%q) succeeded, want error", in)
		}
	}
	// Double slashes normalize deterministically to the same canonical path.
	a, errA := ResolveInput(cwd, "/a//b", nil)
	b, errB := ResolveInput(cwd, "/a/b", nil)
	if errA != nil || errB != nil || a != b {
		t.Errorf("double-slash normalization: %q/%v vs %q/%v", a, errA, b, errB)
	}
}

func TestUnicodeNamesDeterministic(t *testing.T) {
	cwd := mustPath(t, "/home/alice")
	composed := "caf\u00e9"    // é as single code point
	decomposed := "cafe\u0301" // e + combining acute
	if composed == decomposed {
		t.Fatal("test setup: composed and decomposed must differ as strings")
	}
	a, err := ResolveInput(cwd, composed, nil)
	if err != nil {
		t.Fatalf("composed: %v", err)
	}
	b, err := ResolveInput(cwd, decomposed, nil)
	if err != nil {
		t.Fatalf("decomposed: %v", err)
	}
	if a == b {
		t.Fatal("composed and decomposed resolved identically: silent collision")
	}
	// Both are storable and distinct map keys.
	m := map[string]int{string(a): 1, string(b): 2}
	if len(m) != 2 {
		t.Fatal("unicode keys collided in map")
	}
}
