package presentation

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Presentation defaults for the VibeOS/VibeShell identity. These
// are simulated facts: nothing in this package reads the real
// container kernel, hostname, process list, resource usage, or
// environment (PLAN 5.4).
const (
	DefaultSystemName   = "VibeOS"
	DefaultShellName    = "VibeShell"
	DefaultHostname     = "vibeos"
	DefaultRelease      = "1.0.0"
	DefaultMachine      = "x86_64"
	DefaultVersionTag   = "GNU/Hurd-style"
	MaxIdentityFieldLen = 64
)

// hostnamePattern accepts the usual hostname characters so an
// administrator override stays a plausible machine name.
var hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// SystemIdentity holds the simulated machine and shell identity
// presented in uname, OS-release-style views, and the MOTD. All
// fields are configuration data; the zero value is invalid and
// must be replaced by DefaultSystemIdentity() or a validated
// administrator override.
type SystemIdentity struct {
	// SystemName is the operating-system name ("VibeOS").
	SystemName string `json:"system_name"`
	// ShellName is the shell identification ("VibeShell").
	ShellName string `json:"shell_name"`
	// Hostname is the simulated nodename, never the real host.
	Hostname string `json:"hostname"`
	// Release is the simulated release version.
	Release string `json:"release"`
	// Machine is the simulated machine hardware name.
	Machine string `json:"machine"`
	// VersionTag is the uname version field describing the
	// presentation style. It never claims a particular Debian
	// release or a complete GNU/Hurd implementation.
	VersionTag string `json:"version_tag"`
}

// DefaultSystemIdentity returns the VibeOS/VibeShell identity.
func DefaultSystemIdentity() SystemIdentity {
	return SystemIdentity{
		SystemName: DefaultSystemName,
		ShellName:  DefaultShellName,
		Hostname:   DefaultHostname,
		Release:    DefaultRelease,
		Machine:    DefaultMachine,
		VersionTag: DefaultVersionTag,
	}
}

// Validate reports whether every field is present, printable, and
// within bounds. Overrides must pass validation before use so a
// misconfigured identity fails loudly instead of rendering empty
// or control-bearing output.
func (i SystemIdentity) Validate() error {
	for name, value := range map[string]string{
		"system_name": i.SystemName,
		"shell_name":  i.ShellName,
		"release":     i.Release,
		"machine":     i.Machine,
		"version_tag": i.VersionTag,
	} {
		if value == "" {
			return errIdentityField("identity field %q is empty", name)
		}
		if len(value) > MaxIdentityFieldLen {
			return errIdentityField("identity field %q exceeds %d bytes", name, MaxIdentityFieldLen)
		}
		if strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return errIdentityField("identity field %q contains control characters", name)
		}
	}
	if i.Hostname == "" {
		return errIdentityField("identity field %q is empty", "hostname")
	}
	if len(i.Hostname) > MaxIdentityFieldLen {
		return errIdentityField("identity field %q exceeds %d bytes", "hostname", MaxIdentityFieldLen)
	}
	if !hostnamePattern.MatchString(i.Hostname) {
		return errIdentityField("identity field %q is not a plausible hostname: %q", "hostname", i.Hostname)
	}
	return nil
}

func errIdentityField(format string, args ...any) error {
	return fmt.Errorf("invalid system identity: "+format, args...)
}

// Sysname returns the uname -s value (the system name).
func (i SystemIdentity) Sysname() string { return i.SystemName }

// Nodename returns the uname -n value (the simulated hostname).
func (i SystemIdentity) Nodename() string { return i.Hostname }

// Uname returns the uname -a-style line:
// "<system> <nodename> <release> <version> <machine>". It
// reports only the simulated identity.
func (i SystemIdentity) Uname() string {
	return fmt.Sprintf("%s %s %s %s %s", i.SystemName, i.Hostname, i.Release, i.VersionTag, i.Machine)
}

// OSRelease returns the /etc/os-release-style view for the
// simulated machine. It deliberately omits ID_LIKE and any
// distribution lineage so the presentation never claims a
// particular real distribution or release.
func (i SystemIdentity) OSRelease() string {
	return fmt.Sprintf(
		"NAME=%q\nVERSION=%q\nID=%s\nPRETTY_NAME=%q\n",
		i.SystemName,
		fmt.Sprintf("%s (%s simulated environment)", i.Release, i.VersionTag),
		strings.ToLower(i.SystemName),
		fmt.Sprintf("%s %s (%s)", i.SystemName, i.Release, i.VersionTag),
	)
}

// ShellIdentification returns the shell name as presented to
// users and programs that ask for the current shell.
func (i SystemIdentity) ShellIdentification() string { return i.ShellName }

// ShellVersionString returns the shell identification with the
// simulated release, for --version-style presentation.
func (i SystemIdentity) ShellVersionString() string {
	return fmt.Sprintf("%s %s", i.ShellName, i.Release)
}
