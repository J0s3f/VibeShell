package interactions

import (
	"strconv"

	"j0s.at/vibeshell/internal/domain"
)

// Limits bounds everything a single interaction may consume (PLAN 6.2: the
// primitives have resource limits and cannot perform I/O). A zero field is
// replaced by the default so a caller cannot accidentally remove a bound by
// leaving a struct literal sparse; Validate rejects negatives.
//
// Limits are enforced when a machine is built (declarations and state), on
// every applied action (buffer growth, search steps, emitted events), and when
// a frame is built (rows and bytes). Breaching a bound yields a domain limit
// error, never a partial update.
type Limits struct {
	MaxBufferBytes   int `json:"max_buffer_bytes"`
	MaxLines         int `json:"max_lines"`
	MaxLineBytes     int `json:"max_line_bytes"`
	MaxBindings      int `json:"max_bindings"`
	MaxChordLength   int `json:"max_chord_length"`
	MaxUndoDepth     int `json:"max_undo_depth"`
	MaxSearchSteps   int `json:"max_search_steps"`
	MaxCount         int `json:"max_count"`
	MaxStatusBytes   int `json:"max_status_bytes"`
	MaxMetadataBytes int `json:"max_metadata_bytes"`
	MaxColumns       int `json:"max_columns"`
	MaxRows          int `json:"max_rows"`
	MaxCellBytes     int `json:"max_cell_bytes"`
	MaxFields        int `json:"max_fields"`
	MaxFieldBytes    int `json:"max_field_bytes"`
	MaxEventsPerKey  int `json:"max_events_per_key"`
	MaxEventBytes    int `json:"max_event_bytes"`
	MaxFrameRows     int `json:"max_frame_rows"`
	MaxFrameColumns  int `json:"max_frame_columns"`
	MaxExtensionText int `json:"max_extension_text_bytes"`
}

// DefaultLimits returns the bounds the runtime uses when a caller supplies
// none. They are sized for an interactive screen over a simulated world, not
// for bulk data.
func DefaultLimits() Limits {
	return Limits{
		MaxBufferBytes:   1 << 20, // 1 MiB of editable text
		MaxLines:         100_000,
		MaxLineBytes:     64 << 10,
		MaxBindings:      256,
		MaxChordLength:   4,
		MaxUndoDepth:     64,
		MaxSearchSteps:   200_000,
		MaxCount:         1_000_000,
		MaxStatusBytes:   512,
		MaxMetadataBytes: 64 << 10,
		MaxColumns:       64,
		MaxRows:          100_000,
		MaxCellBytes:     8 << 10,
		MaxFields:        64,
		MaxFieldBytes:    8 << 10,
		MaxEventsPerKey:  8,
		MaxEventBytes:    1 << 20,
		MaxFrameRows:     512,
		MaxFrameColumns:  4_096,
		MaxExtensionText: 512,
	}
}

// withDefaults fills unset fields from DefaultLimits.
func (l Limits) withDefaults() Limits {
	defaults := DefaultLimits()
	if l.MaxBufferBytes <= 0 {
		l.MaxBufferBytes = defaults.MaxBufferBytes
	}
	if l.MaxLines <= 0 {
		l.MaxLines = defaults.MaxLines
	}
	if l.MaxLineBytes <= 0 {
		l.MaxLineBytes = defaults.MaxLineBytes
	}
	if l.MaxBindings <= 0 {
		l.MaxBindings = defaults.MaxBindings
	}
	if l.MaxChordLength <= 0 {
		l.MaxChordLength = defaults.MaxChordLength
	}
	if l.MaxUndoDepth <= 0 {
		l.MaxUndoDepth = defaults.MaxUndoDepth
	}
	if l.MaxSearchSteps <= 0 {
		l.MaxSearchSteps = defaults.MaxSearchSteps
	}
	if l.MaxCount <= 0 {
		l.MaxCount = defaults.MaxCount
	}
	if l.MaxStatusBytes <= 0 {
		l.MaxStatusBytes = defaults.MaxStatusBytes
	}
	if l.MaxMetadataBytes <= 0 {
		l.MaxMetadataBytes = defaults.MaxMetadataBytes
	}
	if l.MaxColumns <= 0 {
		l.MaxColumns = defaults.MaxColumns
	}
	if l.MaxRows <= 0 {
		l.MaxRows = defaults.MaxRows
	}
	if l.MaxCellBytes <= 0 {
		l.MaxCellBytes = defaults.MaxCellBytes
	}
	if l.MaxFields <= 0 {
		l.MaxFields = defaults.MaxFields
	}
	if l.MaxFieldBytes <= 0 {
		l.MaxFieldBytes = defaults.MaxFieldBytes
	}
	if l.MaxEventsPerKey <= 0 {
		l.MaxEventsPerKey = defaults.MaxEventsPerKey
	}
	if l.MaxEventBytes <= 0 {
		l.MaxEventBytes = defaults.MaxEventBytes
	}
	if l.MaxFrameRows <= 0 {
		l.MaxFrameRows = defaults.MaxFrameRows
	}
	if l.MaxFrameColumns <= 0 {
		l.MaxFrameColumns = defaults.MaxFrameColumns
	}
	if l.MaxExtensionText <= 0 {
		l.MaxExtensionText = defaults.MaxExtensionText
	}
	return l
}

// Validate rejects explicitly negative bounds, which would otherwise be
// silently replaced by the defaults.
func (l Limits) Validate() error {
	fields := map[string]int{
		"max_buffer_bytes":         l.MaxBufferBytes,
		"max_lines":                l.MaxLines,
		"max_line_bytes":           l.MaxLineBytes,
		"max_bindings":             l.MaxBindings,
		"max_chord_length":         l.MaxChordLength,
		"max_undo_depth":           l.MaxUndoDepth,
		"max_search_steps":         l.MaxSearchSteps,
		"max_count":                l.MaxCount,
		"max_status_bytes":         l.MaxStatusBytes,
		"max_metadata_bytes":       l.MaxMetadataBytes,
		"max_columns":              l.MaxColumns,
		"max_rows":                 l.MaxRows,
		"max_cell_bytes":           l.MaxCellBytes,
		"max_fields":               l.MaxFields,
		"max_field_bytes":          l.MaxFieldBytes,
		"max_events_per_key":       l.MaxEventsPerKey,
		"max_event_bytes":          l.MaxEventBytes,
		"max_frame_rows":           l.MaxFrameRows,
		"max_frame_columns":        l.MaxFrameColumns,
		"max_extension_text_bytes": l.MaxExtensionText,
	}
	for name, value := range fields {
		if value < 0 {
			return domain.NewValidationError(domain.CodeInvalidInput,
				"interaction limit must not be negative", map[string]string{"limit": name})
		}
	}
	return nil
}

// limitExceeded reports a breached bound as a domain limit error carrying the
// offending bound so an operator can see which one stopped the interaction.
func limitExceeded(bound string, actual, allowed int64) error {
	return domain.NewLimitError(domain.CodeOutputTooLarge,
		"interaction resource limit exceeded",
		map[string]string{
			"limit":   bound,
			"actual":  strconv.FormatInt(actual, 10),
			"allowed": strconv.FormatInt(allowed, 10),
		})
}

// exceeded reports whether actual is over the allowed bound.
func exceeded(actual, allowed int64) bool { return actual > allowed }
