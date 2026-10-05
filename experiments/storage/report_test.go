package storage

import (
	"fmt"
	"strings"
	"testing"
)

// report builds the readable body written next to a gate's assertions, so the
// evidence survives without re-reading the test source.
type report struct {
	title string
	lines []string
}

func newReport(title string) *report {
	return &report{title: title, lines: []string{title, strings.Repeat("=", len(title))}}
}

func (r *report) addf(format string, args ...any) *report {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
	return r
}

func (r *report) add(label string, value any) *report {
	return r.addf("%-26s %v", label, value)
}

func (r *report) section(title string) *report {
	r.lines = append(r.lines, "", title)
	return r
}

func (r *report) addAll(label string, values []string) *report {
	r.addf("%-26s %d entries", label, len(values))
	for _, value := range values {
		r.lines = append(r.lines, "  "+value)
	}
	return r
}

func (r *report) write(t *testing.T, name string) {
	t.Helper()
	Receipt(t, name, r.lines...)
}
