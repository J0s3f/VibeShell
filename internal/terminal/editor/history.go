package editor

// History holds accepted command lines in submission order so they can be
// navigated with the up and down arrows. It is bounded: a session that runs for
// hours must not accumulate every line it ever accepted.
type History struct {
	entries []string
	limit   int
}

// DefaultHistoryLimit is how many accepted lines one session keeps.
const DefaultHistoryLimit = 1000

// NewHistory returns an empty history with the given bound. A limit below one
// is raised to one, so a history always remembers the last accepted line.
func NewHistory(limit int) *History {
	if limit < 1 {
		limit = 1
	}
	return &History{limit: limit}
}

// Append records an accepted line. A blank line is not a command worth
// recalling, and a line identical to the previous one would make one arrow
// press look like nothing happened, so both are refused.
func (h *History) Append(entry string) {
	if isBlank(entry) {
		return
	}
	if len(h.entries) > 0 && h.entries[len(h.entries)-1] == entry {
		return
	}
	h.entries = append(h.entries, entry)
	if overflow := len(h.entries) - h.limit; overflow > 0 {
		h.entries = append([]string(nil), h.entries[overflow:]...)
	}
}

// Len returns how many lines the history holds.
func (h *History) Len() int { return len(h.entries) }

// Entries returns the recorded lines, oldest first.
func (h *History) Entries() []string { return append([]string(nil), h.entries...) }

// At returns the entry recorded back positions before the newest one. The
// caller walks backwards while navigating older lines.
func (h *History) At(back int) (string, bool) {
	if back < 0 {
		return "", false
	}
	index := len(h.entries) - 1 - back
	if index < 0 {
		return "", false
	}
	return h.entries[index], true
}

// Clear forgets every entry.
func (h *History) Clear() { h.entries = nil }

func isBlank(s string) bool {
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
		default:
			return false
		}
	}
	return true
}
