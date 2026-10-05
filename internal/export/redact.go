package export

import (
	"encoding/json"
	"strings"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/observability"
)

// defaultRedactionMarker matches observability.Redactor's hardcoded marker so
// a policy that leaves ReplacementText empty behaves exactly like the logging
// redactor.
const defaultRedactionMarker = "[REDACTED]"

// redactor applies an export's domain.RedactionPolicy. Secret-shape detection
// is delegated to observability.Redactor; this type only decides where the
// redaction is applied. It never mutates the caller's records: apply returns a
// record whose payload, blobs, and inline envelope payload are redacted copies.
type redactor struct {
	policy domain.RedactionPolicy
	r      *observability.Redactor
	marker string
	// redactSecrets is cached because it is read on every record.
	secrets bool
}

func newRedactor(policy domain.RedactionPolicy) *redactor {
	marker := policy.ReplacementText
	if marker == "" {
		marker = defaultRedactionMarker
	}
	return &redactor{
		policy:  policy,
		r:       observability.NewRedactor(),
		marker:  marker,
		secrets: policy.RedactSecrets,
	}
}

func (rd *redactor) enabled() bool {
	return rd.policy.RedactSecrets || rd.policy.RedactPayloads || rd.policy.RedactUserInput
}

// apply returns rec with the policy applied. When the envelope carried an
// inline payload it is replaced with the redacted bytes too: the JSONL bundle
// writes the whole envelope, so redacting only the sibling payload field would
// leave the original secret inside envelope.payload.inline.
func (rd *redactor) apply(rec record) record {
	if !rd.enabled() {
		return rec
	}
	if rd.secrets {
		rec.payload = rd.json(rec.payload)
		for id, blob := range rec.blobs {
			rec.blobs[id] = rd.bytes(blob)
		}
	}
	if rd.policy.RedactPayloads && isModelOrToolKind(rec.env.Kind) {
		rec.payload = rd.markerJSON()
	}
	if rd.policy.RedactUserInput && isInputKind(rec.env.Kind) {
		rec.payload = rd.markerJSON()
	}
	if len(strings.TrimSpace(string(rec.env.Payload.Inline))) > 0 {
		rec.env.Payload.Inline = rec.payload
	}
	return rec
}

// leaf redacts one string. The observability redactor substitutes its own
// "[REDACTED]" marker; a non-default policy marker is substituted afterwards.
func (rd *redactor) leaf(s string) string {
	out := rd.r.RedactString(s)
	if rd.marker != defaultRedactionMarker {
		out = strings.ReplaceAll(out, defaultRedactionMarker, rd.marker)
	}
	return out
}

// json redacts every string leaf of a JSON payload. When the payload does not
// decode as JSON, or no leaf changed, the original bytes are returned
// untouched so an export without secrets keeps its exact recorded bytes.
func (rd *redactor) json(raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		if s := rd.leaf(string(raw)); s != string(raw) {
			return []byte(s)
		}
		return raw
	}
	redacted, changed := redactJSONValue(v, rd.leaf)
	if !changed {
		return raw
	}
	out, err := json.Marshal(redacted)
	if err != nil {
		return raw
	}
	return out
}

// bytes redacts a content blob. Terminal output often carries user-typed
// secrets; the same policy applies to blobs so they are covered by the
// manifest checksums after redaction. Binary-safe: the bytes round-trip
// through string without modification when nothing matched.
func (rd *redactor) bytes(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	s := rd.leaf(string(b))
	if s == string(b) {
		return b
	}
	return []byte(s)
}

func (rd *redactor) markerJSON() []byte {
	b, err := json.Marshal(map[string]any{"redacted": true, "marker": rd.marker})
	if err != nil {
		return []byte(`{"redacted":true}`)
	}
	return b
}

func redactJSONValue(v any, leaf func(string) string) (any, bool) {
	switch t := v.(type) {
	case string:
		next := leaf(t)
		return next, next != t
	case []any:
		changed := false
		for i := range t {
			next, c := redactJSONValue(t[i], leaf)
			t[i] = next
			changed = changed || c
		}
		return t, changed
	case map[string]any:
		changed := false
		for k := range t {
			next, c := redactJSONValue(t[k], leaf)
			t[k] = next
			changed = changed || c
		}
		return t, changed
	default:
		return v, false
	}
}

func isModelOrToolKind(kind domain.EventKind) bool {
	switch kind {
	case domain.EventKindModelRequest, domain.EventKindModelResponse, domain.EventKindModelError,
		domain.EventKindToolRequest, domain.EventKindToolResult, domain.EventKindToolError:
		return true
	default:
		return false
	}
}

func isInputKind(kind domain.EventKind) bool {
	switch kind {
	case domain.EventKindInputRaw, domain.EventKindInputDecoded, domain.EventKindInputAccepted:
		return true
	default:
		return false
	}
}
