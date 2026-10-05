package interactions

import (
	"strings"
	"unicode"
)

// searchOutcome is what a search primitive produced.
type searchOutcome struct {
	// Cursor is where the cursor lands: the first character of the match for a
	// forward search, the last character for a backward search.
	Cursor Cursor
	// Match is the hit, or nil when the pattern was not found.
	Match *Match
	// Status is the bounded note a spec-independent primitive sets when there is
	// nothing to report (an empty undo stack, a missing pattern). It is
	// deliberately generic: application wording belongs to the application.
	Status string
}

// statusNotes are the fixed notes the primitives own. Anything specific to an
// application is emitted as a semantic event and worded by its handler.
const (
	statusNothingToUndo   = "nothing to undo"
	statusNothingToRedo   = "nothing to redo"
	statusPatternNotFound = "pattern not found"
	statusNoPattern       = "no search pattern"
)

// search scans the document for a literal pattern.
//
// Scanning is bounded by Limits.MaxSearchSteps lines, so a hostile or enormous
// buffer cannot turn one key press into unbounded work. Matching is literal, so
// a pattern carries no code and cannot backtrack catastrophically. Each search
// resumes from the cursor within its own line and, when asked to wrap, continues
// from the other end, so repeating a search walks successive matches instead of
// landing on the same one.
func search(lines []string, from Cursor, pattern string, direction SearchDirection, caseMode CaseMode, limits Limits) (searchOutcome, error) {
	trimmed := strings.TrimSpace(pattern)
	if trimmed == "" {
		return searchOutcome{Status: statusNoPattern}, nil
	}
	wrap := direction == SearchForwardWrap || direction == SearchBackwardWrap
	forward := direction == SearchForward || direction == SearchForwardWrap
	caseSensitive := caseMode != CaseInsensitive

	if len(lines) == 0 {
		lines = []string{""}
	}
	steps := 0
	// scan walks lines in the search direction. The first line is searched only
	// outside the cursor position; every other line is searched whole.
	scan := func(firstLine int, delta int, firstFrom, firstTo int) (*Match, error) {
		for i := firstLine; i >= 0 && i < len(lines); i += delta {
			steps++
			if exceeded(int64(steps), int64(limits.MaxSearchSteps)) {
				return nil, limitExceeded("max_search_steps", int64(steps), int64(limits.MaxSearchSteps))
			}
			from, to := 0, len([]rune(lines[i]))
			if i == firstLine {
				from, to = firstFrom, firstTo
			}
			if hit, ok := findInRange(lines[i], trimmed, caseSensitive, from, to); ok {
				return &Match{Line: i, Start: hit[0], End: hit[1]}, nil
			}
		}
		return nil, nil
	}

	startLine := from.Line
	startFrom, startTo := 0, len([]rune(lineAt(lines, from.Line)))
	if forward {
		// Resume after the cursor so the next match is a later one.
		startFrom = min(from.Column+1, startTo)
	} else {
		// Resume before the cursor, which also excludes the match the cursor is
		// sitting on when a backward search is repeated.
		startTo = max(0, min(from.Column, startTo))
	}
	hit, err := scan(startLine, searchDelta(forward), startFrom, startTo)
	if err != nil {
		return searchOutcome{}, err
	}
	if hit == nil && wrap {
		hit, err = scan(0, searchDelta(forward), 0, len([]rune(lineAt(lines, 0))))
		if err != nil {
			return searchOutcome{}, err
		}
	}
	if hit == nil {
		return searchOutcome{Status: statusPatternNotFound}, nil
	}
	column := hit.Start
	if !forward {
		column = hit.End - 1
	}
	if column < 0 {
		column = 0
	}
	return searchOutcome{Cursor: Cursor{Line: hit.Line, Column: column}, Match: hit}, nil
}

func searchDelta(forward bool) int {
	if forward {
		return 1
	}
	return -1
}

func lineAt(lines []string, index int) string {
	if index < 0 || index >= len(lines) {
		return ""
	}
	return lines[index]
}

// findInRange returns the rune offsets of the first literal occurrence of
// pattern in line within [from, to), honouring case sensitivity.
func findInRange(line, pattern string, caseSensitive bool, from, to int) ([2]int, bool) {
	haystack, needle := []rune(line), []rune(pattern)
	if len(needle) == 0 {
		return [2]int{}, false
	}
	from = max(0, from)
	to = min(len(haystack), to)
	if to-from < len(needle) {
		return [2]int{}, false
	}
	for start := from; start+len(needle) <= to; start++ {
		if runesEqual(haystack[start:start+len(needle)], needle, caseSensitive) {
			return [2]int{start, start + len(needle)}, true
		}
	}
	return [2]int{}, false
}

func runesEqual(a, b []rune, caseSensitive bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if caseSensitive {
			if a[i] != b[i] {
				return false
			}
			continue
		}
		if unicode.ToLower(a[i]) != unicode.ToLower(b[i]) {
			return false
		}
	}
	return true
}

// resolveQuery returns the pattern a search should use: the binding's literal
// pattern, the text the application collected in its prompt line, or the pattern
// of the previous successful search.
func resolveQuery(p SearchParams, state State) string {
	switch p.Query {
	case QueryLiteral:
		return p.Literal
	case QueryLast:
		return state.LastQuery
	default:
		return state.PromptText
	}
}

// matchAt reports whether a search hit still matches the recorded state, so a
// selection that refers to "match_start" fails cleanly after edits.
func matchAt(lines []string, match *Match, caseMode CaseMode) bool {
	if match == nil || match.Line < 0 || match.Line >= len(lines) {
		return false
	}
	runes := []rune(lines[match.Line])
	if match.Start < 0 || match.End > len(runes) || match.Start >= match.End {
		return false
	}
	pattern := string(runes[match.Start:match.End])
	found, ok := findInRange(lines[match.Line], pattern, caseMode != CaseInsensitive, match.Start, match.End+1)
	return ok && found == [2]int{match.Start, match.End}
}
