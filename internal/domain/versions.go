package domain

// Versioned contract identifiers shared by Go code and the JSON Schemas in
// schemas/. Every schema file carries a matching "version" constant so a
// reader can tell which revision produced any persisted or exported record.
// Bump the constant and the schema together; never one without the other.
const (
	// EventContractVersion matches schemas/event-envelope.schema.json and
	// EventSchemaVersion above.
	EventContractVersion = 1
	// ChangeSetContractVersion matches schemas/world-changeset.schema.json.
	ChangeSetContractVersion = 1
	// AppContractVersion matches schemas/app-abi.schema.json and AppABIVersion.
	AppContractVersion = 1
	// ModelContractVersion matches schemas/model-request-result.schema.json.
	ModelContractVersion = 1
	// ConfigContractVersion matches schemas/configuration.schema.json.
	ConfigContractVersion = 1
	// RoutePolicyContractVersion matches schemas/route-policy.schema.json.
	RoutePolicyContractVersion = 1
)
