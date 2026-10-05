package interactions

import (
	"strconv"
	"strings"
)

// statusFields are the closed set of values a declared status line may show.
// Substitution is plain replacement from this set: a status line cannot compute,
// read state a view did not declare, or carry anything the renderer would have to
// escape. Declaring the dirty marker here is what lets a view show unsaved
// changes without every application writing its own status update.
var statusFields = map[string]bool{
	"mode":    true,
	"status":  true,
	"dirty":   true,
	"line":    true,
	"lines":   true,
	"column":  true,
	"total":   true,
	"percent": true,
	"sort":    true,
}

// modifiedMarker is what {dirty} shows for an unsaved document.
const modifiedMarker = "[+]"

// statusViewValues are the numbers a status line may show, resolved once per
// frame from the interaction state.
type statusViewValues struct {
	Mode    string
	Status  string
	Dirty   bool
	Line    int
	Lines   int
	Column  int
	Total   int
	Percent int
	Sort    string
}

// statusValues resolves the substitution values for the current state.
func (m *Machine) statusValues() statusViewValues {
	cursor := m.focusCursor()
	lines := m.contentLines()
	percent := 100
	if lines > 0 && m.pageRows() > 0 && lines > m.pageRows() {
		percent = m.state.Viewport.Top * 100 / max(1, lines-m.pageRows())
	}
	sort := ""
	if column := m.state.Table.Sort.Column; column != "" {
		sort = column
		if m.state.Table.Sort.Order != "" {
			sort += " " + string(m.state.Table.Sort.Order)
		}
	}
	return statusViewValues{
		Mode:    m.state.Mode,
		Status:  m.state.Status,
		Dirty:   m.state.Dirty,
		Line:    cursor.Line + 1,
		Lines:   lines,
		Column:  cursor.Column + 1,
		Total:   len(m.spec.View.Columns),
		Percent: percent,
		Sort:    sort,
	}
}

// substituteStatus replaces every declared placeholder in a status line. Unknown
// placeholders are left as written; Spec.Validate rejects them, so a live
// interaction never reaches this path with one.
func substituteStatus(line string, values statusViewValues) string {
	if !strings.Contains(line, "{") {
		return line
	}
	var out strings.Builder
	for i := 0; i < len(line); {
		if line[i] != '{' {
			out.WriteByte(line[i])
			i++
			continue
		}
		end := strings.IndexByte(line[i:], '}')
		if end < 0 {
			out.WriteString(line[i:])
			break
		}
		name := line[i+1 : i+end]
		if !statusFields[name] {
			out.WriteString(line[i : i+end+1])
			i += end + 1
			continue
		}
		out.WriteString(statusFieldValue(name, values))
		i += end + 1
	}
	return out.String()
}

func statusFieldValue(name string, values statusViewValues) string {
	switch name {
	case "mode":
		return values.Mode
	case "status":
		return values.Status
	case "dirty":
		if values.Dirty {
			return modifiedMarker
		}
		return ""
	case "line":
		return strconv.Itoa(values.Line)
	case "lines":
		return strconv.Itoa(values.Lines)
	case "column":
		return strconv.Itoa(values.Column)
	case "total":
		return strconv.Itoa(values.Total)
	case "percent":
		return strconv.Itoa(values.Percent) + "%"
	case "sort":
		return values.Sort
	default:
		return ""
	}
}

// validateStatusLine rejects a declared status line with an unknown
// placeholder, which would otherwise reach a user as literal braces.
func validateStatusLine(line string) error {
	for i := 0; i < len(line); {
		if line[i] != '{' {
			i++
			continue
		}
		end := strings.IndexByte(line[i:], '}')
		if end < 0 {
			return specError("status line %q has an unclosed placeholder", line)
		}
		name := line[i+1 : i+end]
		if !statusFields[name] {
			return specError("status line placeholder {%s} is not one of the declared fields", name)
		}
		i += end + 1
	}
	return nil
}

// statusText is the status line shown under the frame: the interaction's own
// message when it has one, otherwise the text the view declared. Both are
// sanitized, because both can carry application data.
func (m *Machine) statusText() string {
	values := m.statusValues()
	line := m.spec.View.StatusLine
	if m.state.Status != "" {
		line = m.state.Status
	}
	if line == "" {
		return ""
	}
	substituted := substituteStatus(line, values)
	if exceeded(int64(len(substituted)), int64(m.limits.MaxStatusBytes)) {
		substituted = truncateRunes(substituted, m.limits.MaxStatusBytes)
	}
	return SanitizeData(substituted)
}
