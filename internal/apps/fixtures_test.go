package apps

import (
	"encoding/json"
	"fmt"

	"j0s.at/vibeshell/internal/domain"
)

// testManifest builds a valid manifest whose state
// schema declares the given schema version.
func testManifest(command string, schemaVersion int64) domain.AppManifest {
	return domain.AppManifest{
		ABIVersion:   domain.AppABIVersion,
		CommandNames: []string{command},
		Description:  "test application " + command,
		StateSchema:  json.RawMessage(fmt.Sprintf(`{"version":%d}`, schemaVersion)),
		Capabilities: []string{domain.CapabilityFileRead},
		Entrypoint:   "handle",
		Version:      "1.0.0",
	}
}

// testSource is a syntactically plausible JavaScript
// candidate source.
func testSource() string {
	return "export function handle(event, state) {\n" +
		"  return { state: state, view: { mode: 'text' } };\n" +
		"}\n"
}

// userState builds a user-scoped state portion at the
// given schema version carrying an opaque payload.
func userState(schemaVersion int64, data string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"schema":%d,"data":%q}`, schemaVersion, data))
}
