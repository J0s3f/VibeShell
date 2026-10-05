package storage

import "strings"

// PathKey is the indexed form of a path for prefix queries: the path followed
// by the separator. Every descendant of p has a key that starts with p + "/",
// which is what makes boundary-aware matching possible without string surgery
// on the query side.
func PathKey(path string) string {
	return strings.TrimSuffix(path, "/") + "/"
}

// SubtreeRange returns the half-open key range covering prefix itself and every
// path below it. Because a normalized path never contains a "." or ".."
// component and never ends in a separator, "/" is the smallest byte that can
// follow a complete path name, so every key of prefix and its descendants is at
// least prefix+"/" and smaller than prefix+"0", while a sibling whose name merely
// starts with the same characters (for example "/somewhere" for prefix "/some")
// sorts after the range and is excluded.
//
// The empty prefix, the namespace root, yields ["/", "0"), which covers the
// whole tree.
func SubtreeRange(prefix string) (low, high string) {
	trimmed := strings.TrimSuffix(prefix, "/")
	return trimmed + "/", trimmed + "0"
}

// EscapeLikePrefix escapes the LIKE wildcards in a path prefix so that a
// directory named "100%" or "a_b" is matched literally. It is needed only by the
// LIKE variant of the prefix query; the key range above needs no escaping.
func EscapeLikePrefix(prefix string) string {
	var out strings.Builder
	out.Grow(len(prefix))
	for _, r := range prefix {
		if r == '%' || r == '_' || r == '\\' {
			out.WriteRune('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}

// NormalizeCommand is the grouping key for the normalized command index: the
// command line with surrounding whitespace removed and internal whitespace runs
// collapsed to one space, so the same invocation typed with different spacing
// shares a row.
//
// Case is deliberately preserved. A Unix command name is case-sensitive, so
// lowercasing would group two different commands together. Any future
// case-folded search belongs in the full-text projection, not here.
func NormalizeCommand(command string) string {
	return strings.Join(strings.Fields(command), " ")
}
