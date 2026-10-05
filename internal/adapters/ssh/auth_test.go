package ssh_test

import (
	"context"
	"strings"
	"testing"

	adapter "j0s.at/vibeshell/internal/adapters/ssh"
)

func noopHandler() adapter.Handler {
	return adapter.HandlerFunc(func(context.Context, *adapter.Session) {})
}

func TestValidateUsername(t *testing.T) {
	valid := []string{"alice", "bob123", "a", "müller", "user-name_1.x", "Zoom_42"}
	for _, name := range valid {
		if err := adapter.ValidateUsername(name); err != nil {
			t.Errorf("ValidateUsername(%q) = %v, want nil", name, err)
		}
	}
	invalid := map[string]string{
		"":                         "empty",
		"a/b":                      "path separator",
		`a\b`:                      "path separator",
		"a b":                      "space",
		"a\tb":                     "control",
		"a\nb":                     "control",
		"a\x1bb":                   "control",
		"trailing ":                "space",
		"\x7f":                     "control",
		string([]byte{0xff, 0xfe}): "not valid UTF-8",
		strings.Repeat("a", 65):    "longer than",
		// 64 bytes total: passes the length bound, fails on the space.
		strings.Repeat("a", 63) + " ": "control or space",
	}
	for name, fragment := range invalid {
		err := adapter.ValidateUsername(name)
		if err == nil {
			t.Errorf("ValidateUsername(%q) = nil, want an error", name)
			continue
		}
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("ValidateUsername(%q) = %v, want it to mention %q", name, err, fragment)
		}
	}
}

func TestNewServerRejectsBadOptions(t *testing.T) {
	passwords := adapter.PasswordAuthenticatorFunc(
		func(username, password string) (adapter.Principal, error) {
			return adapter.Principal{Username: username}, nil
		})

	cases := map[string]adapter.Options{
		"nil handler":              {Mode: adapter.ModePublic},
		"public with passwords":    {Mode: adapter.ModePublic, Passwords: passwords, Handler: noopHandler()},
		"password without backend": {Mode: adapter.ModePassword, Handler: noopHandler()},
		"unknown mode":             {Mode: adapter.Mode(42), Handler: noopHandler()},
		"zero handshake timeout": {Mode: adapter.ModePublic, Handler: noopHandler(),
			Limits: adapter.Limits{HandshakeTimeout: -1}},
		"inverted dimensions": {Mode: adapter.ModePublic, Handler: noopHandler(),
			Limits: adapter.Limits{MinDimension: 80, MaxDimension: 24}},
		"zero output queue": {Mode: adapter.ModePublic, Handler: noopHandler(),
			Limits: adapter.Limits{MaxOutputQueueBytes: -5}},
	}
	for name, opts := range cases {
		if _, err := adapter.NewServer(opts); err == nil {
			t.Errorf("NewServer(%s) = nil error, want a validation error", name)
		}
	}

	if _, err := adapter.NewServer(adapter.Options{
		Mode: adapter.ModePublic, Handler: noopHandler(),
	}); err != nil {
		t.Errorf("NewServer(public defaults) = %v, want nil", err)
	}
	if _, err := adapter.NewServer(adapter.Options{
		Mode: adapter.ModePassword, Passwords: passwords, Handler: noopHandler(),
	}); err != nil {
		t.Errorf("NewServer(password defaults) = %v, want nil", err)
	}
}
