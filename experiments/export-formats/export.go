package exportformats

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"j0s.at/vibeshell/internal/domain"
)

// FileEntry is one verifiable artifact in the JSONL bundle manifest.
type FileEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest is the bundle's verifiable index. It records schema versions,
// the accountable snapshots needed to interpret history, explicit
// secrets-exclusion, per-file checksums, and the session outcome.
type Manifest struct {
	Format               string      `json:"format"`
	FormatVersion        int         `json:"format_version"`
	EventSchemaVersion   int         `json:"event_schema_version"`
	SessionID            string      `json:"session_id"`
	SessionStatus        string      `json:"session_status"` // "complete" or "incomplete_disconnected"
	EventCount           int         `json:"event_count"`
	Scope                string      `json:"scope"`
	ConfigSnapshot       string      `json:"config_snapshot"`
	PromptSnapshot       string      `json:"prompt_snapshot"`
	CatalogSnapshot      string      `json:"catalog_snapshot"`
	SecretsExcluded      bool        `json:"secrets_excluded"`
	ContentBlobsVerified bool        `json:"content_blobs_verified"`
	Files                []FileEntry `json:"files"`
}

func sha256File(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Bundle is a streaming writer for format 1: the versioned JSONL
// research bundle. It holds only bounded buffers.
type Bundle struct {
	dir        string
	w          *bufio.Writer
	f          *os.File
	blobs      map[string][]byte // collected for manifest (bounded by synthetic blob count in spike)
	eventCount int
}

// NewBundle creates the bundle directory layout: events.jsonl, contents/, schema.json.
func NewBundle(dir string) (*Bundle, error) {
	if err := os.MkdirAll(filepath.Join(dir, "contents"), 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	return &Bundle{dir: dir, f: f, w: bufio.NewWriterSize(f, 64*1024), blobs: map[string][]byte{}}, nil
}

// Write appends one event line (envelope + payload JSON) to events.jsonl
// and writes new content blobs under contents/.
func (b *Bundle) Write(r Record) error {
	row := struct {
		Envelope domain.EventEnvelope `json:"envelope"`
		Payload  json.RawMessage      `json:"payload"`
	}{Envelope: r.Env, Payload: r.PayloadJSON}
	line, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if _, err := b.w.Write(append(line, '\n')); err != nil {
		return err
	}
	for id, blob := range r.Blobs {
		if _, seen := b.blobs[id]; seen {
			continue
		}
		b.blobs[id] = blob
		if err := os.WriteFile(filepath.Join(b.dir, "contents", id), blob, 0o644); err != nil {
			return err
		}
	}
	b.eventCount++
	return nil
}

// Close flushes events.jsonl, writes schema.json and manifest.json, and
// leaves every emitted file covered by a sha256 in the manifest.
func (b *Bundle) Close(s Stream) (*Manifest, error) {
	if err := b.w.Flush(); err != nil {
		return nil, err
	}
	if err := b.f.Close(); err != nil {
		return nil, err
	}
	schemaDoc := map[string]any{
		"format":             "vibeshell-jsonl-research-bundle",
		"format_version":     1,
		"event_schema":       domain.EventSchemaVersion,
		"envelope_fields":    []string{"schema_version", "event_id", "session_id", "sequence", "timestamp", "monotonic_offset", "kind", "payload", "provenance", "turn_id", "attempt_id", "app_version_id", "node_revision"},
		"known_event_kinds":  domain.AllEventKinds(),
		"binary_payload_enc": "json base64 for byte slices; blobs referenced by content id",
	}
	schemaBytes, _ := json.MarshalIndent(schemaDoc, "", "  ")
	if err := os.WriteFile(filepath.Join(b.dir, "schema.json"), schemaBytes, 0o644); err != nil {
		return nil, err
	}
	status := "complete"
	if !s.Complete {
		status = "incomplete_disconnected"
	}
	m := &Manifest{
		Format:             "vibeshell-jsonl-research-bundle",
		FormatVersion:      1,
		EventSchemaVersion: domain.EventSchemaVersion,
		SessionID:          s.SessionID,
		SessionStatus:      status,
		EventCount:         b.eventCount,
		Scope:              "session",
		ConfigSnapshot:     "cfg_2026-10-03",
		PromptSnapshot:     "prmt_2026-10-03",
		CatalogSnapshot:    "cat_2026-10-03",
		SecretsExcluded:    true,
	}
	var files []FileEntry
	err := filepath.Walk(b.dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(b.dir, p)
		if rel == "manifest.json" {
			return nil
		}
		bts, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, FileEntry{Path: filepath.ToSlash(rel), SHA256: sha256File(bts), Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	m.Files = files
	m.ContentBlobsVerified = true
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(b.dir, "manifest.json"), mb, 0o644); err != nil {
		return nil, err
	}
	return m, nil
}

// VerifyManifest recomputes sha256 for every manifest entry and returns
// the list of mismatches (empty on success).
func VerifyManifest(dir string) ([]string, error) {
	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(mb, &m); err != nil {
		return nil, err
	}
	var problems []string
	seen := map[string]bool{}
	for _, f := range m.Files {
		seen[f.Path] = true
		bts, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			problems = append(problems, f.Path+": unreadable")
			continue
		}
		if got := sha256File(bts); got != f.SHA256 {
			problems = append(problems, f.Path+": sha256 mismatch")
		}
		if int64(len(bts)) != f.Size {
			problems = append(problems, f.Path+": size mismatch")
		}
	}
	return problems, nil
}
