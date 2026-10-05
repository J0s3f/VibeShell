package export

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"j0s.at/vibeshell/internal/domain"
)

// contentsDir holds one file per emitted content blob, named by its content ID.
const contentsDir = "contents"

// bundleFormatName identifies the JSONL research-bundle layout.
const bundleFormatName = "vibeshell-jsonl-research-bundle"

// FileEntry is one verifiable artifact in the bundle manifest.
type FileEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// SessionEntry is the explicit outcome of one exported session.
type SessionEntry struct {
	SessionID  string `json:"session_id"`
	Status     string `json:"status"` // statusComplete or statusIncompleteDisconnect
	EndReason  string `json:"end_reason,omitempty"`
	EventCount int    `json:"event_count"`
}

// Manifest is the bundle's verifiable index. It records schema versions, the
// accountable snapshots needed to interpret history, the secrets-exclusion
// decision, per-session status, and a sha256 for every emitted file. The
// manifest file itself is not listed: it holds the index and cannot contain its
// own checksum.
type Manifest struct {
	Format               string         `json:"format"`
	FormatVersion        int            `json:"format_version"`
	EventSchemaVersion   int            `json:"event_schema_version"`
	ExporterVersion      string         `json:"exporter_version"`
	Scope                string         `json:"scope"`
	GeneratedAt          int64          `json:"generated_at"`
	Sessions             []SessionEntry `json:"sessions"`
	EventCount           int            `json:"event_count"`
	ConfigSnapshot       string         `json:"config_snapshot,omitempty"`
	PromptSnapshot       string         `json:"prompt_snapshot,omitempty"`
	CatalogSnapshot      string         `json:"catalog_snapshot,omitempty"`
	SecretsExcluded      bool           `json:"secrets_excluded"`
	ContentBlobsVerified bool           `json:"content_blobs_verified"`
	Files                []FileEntry    `json:"files"`
}

// bundle streams format 1: the versioned JSONL research bundle. It holds only
// bounded buffers; emitted blobs are written through immediately and tracked by
// id so a repeated content reference is written once.
type bundle struct {
	dir             string
	f               *os.File
	w               *bufio.Writer
	exporterVersion string
	scope           string
	generatedAt     int64
	secretsExcluded bool
	eventCount      int
	blobsWritten    map[string]struct{}
}

// newBundle creates the directory layout: events.jsonl, contents/, and (at
// close) schema.json and manifest.json.
func newBundle(dir, exporterVersion, scope string, generatedAt int64, secretsExcluded bool) (*bundle, error) {
	if err := os.MkdirAll(filepath.Join(dir, contentsDir), 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	return &bundle{
		dir:             dir,
		f:               f,
		w:               bufio.NewWriterSize(f, 64*1024),
		exporterVersion: exporterVersion,
		scope:           scope,
		generatedAt:     generatedAt,
		secretsExcluded: secretsExcluded,
		blobsWritten:    map[string]struct{}{},
	}, nil
}

func (b *bundle) primaryPath() string { return filepath.Join(b.dir, "events.jsonl") }

// write appends one event line (envelope plus resolved payload) and writes any
// content blobs the record carries.
func (b *bundle) write(rec record) error {
	payload := rec.payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	line, err := json.Marshal(struct {
		Envelope domain.EventEnvelope `json:"envelope"`
		Payload  json.RawMessage      `json:"payload"`
	}{Envelope: rec.env, Payload: payload})
	if err != nil {
		return fmt.Errorf("export: encode bundle event: %w", err)
	}
	if _, err := b.w.Write(append(line, '\n')); err != nil {
		return err
	}
	for id, blob := range rec.blobs {
		if _, ok := b.blobsWritten[id]; ok {
			continue
		}
		if err := os.WriteFile(filepath.Join(b.dir, contentsDir, id), blob, 0o644); err != nil {
			return err
		}
		b.blobsWritten[id] = struct{}{}
	}
	b.eventCount++
	return nil
}

// close flushes events.jsonl, writes schema.json, and writes manifest.json
// covering every emitted file.
func (b *bundle) close(summary streamSummary) error {
	if err := b.w.Flush(); err != nil {
		return err
	}
	if err := b.f.Close(); err != nil {
		return err
	}
	if err := b.writeSchema(); err != nil {
		return err
	}
	files, err := hashTree(b.dir, "manifest.json")
	if err != nil {
		return err
	}
	manifest := Manifest{
		Format:               bundleFormatName,
		FormatVersion:        BundleFormatVersion,
		EventSchemaVersion:   domain.EventSchemaVersion,
		ExporterVersion:      b.exporterVersion,
		Scope:                b.scope,
		GeneratedAt:          b.generatedAt,
		Sessions:             sessionEntries(summary.sessions),
		EventCount:           b.eventCount,
		ConfigSnapshot:       summary.configVersion,
		PromptSnapshot:       summary.promptVersion,
		CatalogSnapshot:      summary.catalogueVersion,
		SecretsExcluded:      b.secretsExcluded,
		ContentBlobsVerified: true,
		Files:                files,
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(b.dir, "manifest.json"), raw, 0o644)
}

func (b *bundle) abort() error { return b.f.Close() }

func (b *bundle) writeSchema() error {
	doc := map[string]any{
		"format":            bundleFormatName,
		"format_version":    BundleFormatVersion,
		"event_schema":      domain.EventSchemaVersion,
		"exporter_version":  b.exporterVersion,
		"envelope_fields":   []string{"schema_version", "event_id", "session_id", "sequence", "timestamp", "monotonic_offset", "kind", "payload", "provenance", "turn_id", "attempt_id", "app_version_id", "node_revision"},
		"known_event_kinds": domain.AllEventKinds(),
		"binary_payload_encoding": "json base64 for byte slices; separately stored blobs are emitted under " +
			contentsDir + "/<content-id> with a sha256 in the manifest",
		"redaction": "when the export redaction policy requests it, string leaves are redacted in place and detected secret shapes are replaced by the policy marker",
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(b.dir, "schema.json"), raw, 0o644)
}

func sessionEntries(statuses []sessionStatus) []SessionEntry {
	entries := make([]SessionEntry, 0, len(statuses))
	for _, st := range statuses {
		status := statusIncompleteDisconnect
		if st.Complete {
			status = statusComplete
		}
		entries = append(entries, SessionEntry{
			SessionID:  st.SessionID.String(),
			Status:     status,
			EndReason:  st.EndReason,
			EventCount: st.EventCount,
		})
	}
	return entries
}

// VerifyBundle recomputes sha256 for every file the manifest lists and reports
// every mismatch, unreadable entry, and unlisted file (except manifest.json,
// which is the index itself). An empty result proves the bundle is intact.
func VerifyBundle(dir string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("export: decode manifest: %w", err)
	}
	var problems []string
	listed := map[string]bool{}
	for _, entry := range manifest.Files {
		listed[entry.Path] = true
		sum, size, err := hashFile(filepath.Join(dir, filepath.FromSlash(entry.Path)))
		if err != nil {
			problems = append(problems, entry.Path+": unreadable")
			continue
		}
		if sum != entry.SHA256 {
			problems = append(problems, entry.Path+": sha256 mismatch")
		}
		if size != entry.Size {
			problems = append(problems, entry.Path+": size mismatch")
		}
	}
	present, err := hashTree(dir, "manifest.json")
	if err != nil {
		return nil, err
	}
	for _, entry := range present {
		if !listed[entry.Path] {
			problems = append(problems, entry.Path+": not listed in manifest")
		}
	}
	return problems, nil
}

// hashTree walks dir and returns a sha256 entry for every regular file except
// skip (a relative path), sorted by path.
func hashTree(dir, skip string) ([]FileEntry, error) {
	var files []FileEntry
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == skip {
			return nil
		}
		sum, size, err := hashFile(path)
		if err != nil {
			return err
		}
		files = append(files, FileEntry{Path: rel, SHA256: sum, Size: size})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// hashFile streams a file through sha256 so hashing a large bundle never holds
// the file in memory.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// fileDigest returns the sha256 and size of one emitted file, used for
// domain.ExportResult.
func fileDigest(path string) (int64, string, error) {
	sum, size, err := hashFile(path)
	if err != nil {
		return 0, "", err
	}
	return size, sum, nil
}
