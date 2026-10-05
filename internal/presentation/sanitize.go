package presentation

import (
	"strings"
	"unicode"
)

// SanitizeMOTD turns raw model output into plain terminal-safe
// text: it strips terminal control sequences (ANSI escape
// sequences, OSC/DCS payloads, and stray control characters),
// removes Markdown code fences, normalizes line endings, and
// drops leading and trailing blank lines. Text shown as data must
// never activate clipboard, hyperlink, title-change, or other
// unapproved terminal controls (PLAN 6.1, 7.4).
func SanitizeMOTD(raw string) string {
	plain := stripControlSequences(raw)
	plain = stripMarkdownFences(plain)
	return normalizeLines(plain)
}

// stripControlSequences removes escape-sequence payloads and all
// C0/C1 control characters except newline and tab. It operates
// on runes so multi-byte UTF-8 text passes through unchanged.
func stripControlSequences(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	runes := []rune(raw)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '':
			i = skipEscapeSequence(runes, i)
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case isForbiddenRune(r):
			// C0 (except \n and \t), DEL, and C1 characters are
			// dropped rather than displayed.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// skipEscapeSequence consumes one escape sequence starting at
// runes[i] == ESC and returns the index of its last rune, so the
// caller's increment advances past it.
func skipEscapeSequence(runes []rune, i int) int {
	if i+1 >= len(runes) {
		return i // lone ESC at end of input
	}
	switch next := runes[i+1]; {
	case next == '[':
		// CSI: parameter bytes 0x30-0x3F, intermediate bytes
		// 0x20-0x2F, then one final byte 0x40-0x7E.
		j := i + 2
		for j < len(runes) && runes[j] >= 0x20 && runes[j] <= 0x3F {
			j++
		}
		if j < len(runes) && runes[j] >= 0x40 && runes[j] <= 0x7E {
			return j
		}
		return j - 1
	case next == ']' || next == 'P' || next == 'X' || next == '^' || next == '_':
		// OSC, DCS, SOS, PM, APC: consume until BEL or the
		// string terminator (ESC \).
		for j := i + 2; j < len(runes); j++ {
			if runes[j] == '' || runes[j] == 0x07 {
				if runes[j] == '' && j+1 < len(runes) && runes[j+1] == '\\' {
					return j + 1
				}
				return j
			}
		}
		return len(runes) - 1
	default:
		// Two-byte escape sequence such as ESC M or ESC 7.
		return i + 1
	}
}

// stripMarkdownFences drops code-fence lines (``` or ~~~ with an
// optional info string) so a fenced model reply renders as its
// content, not as a Markdown artifact.
func stripMarkdownFences(plain string) string {
	lines := strings.Split(plain, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if isMarkdownFence(line) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func isMarkdownFence(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	for _, marker := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, marker) {
			rest := trimmed[len(marker):]
			// Only a bare fence or a fence with an info string.
			if rest == "" || strings.Trim(rest, " \t") == rest && !strings.ContainsAny(rest, "`~") {
				return true
			}
		}
	}
	return false
}

// normalizeLines trims carriage returns, drops leading and
// trailing blank lines, and guarantees no trailing newline.
func normalizeLines(plain string) string {
	lines := strings.Split(plain, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return strings.Join(lines[start:end], "\n")
}

// isForbiddenRune reports whether a rune must never appear in
// presented text: C0 controls except newline and tab, DEL, and
// the C1 range.
func isForbiddenRune(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || unicode.IsControl(r)
}

// applyLineBudget truncates text to at most budget lines and
// reports whether truncation occurred.
func applyLineBudget(text string, budget int) (string, bool) {
	if budget <= 0 {
		return text, false
	}
	lines := strings.Split(text, "\n")
	if len(lines) <= budget {
		return text, false
	}
	return strings.Join(lines[:budget], "\n"), true
}
