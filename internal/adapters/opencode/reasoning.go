package opencode

import (
	"regexp"
	"strings"
)

// Reasoning models sometimes emit their chain of thought inside the content
// channel instead of a separate reasoning_content field. The canonical
// response carries the model's answer, not its private reasoning, so those
// blocks are removed before the response leaves the adapter. A well-delimited
// block is removed in place; an unclosed block means the response was cut
// inside the reasoning, so everything from the marker on is dropped because no
// answer follows it.
//
// The tags are assembled from parts so no XML-like literal appears in this
// file, and the brain-emoji marker is written as a rune rather than a literal
// so the pattern can never collapse to an empty alternative.
const (
	thinkOpen     = "<" + "think" + ">"
	thinkClose    = "<" + "/" + "think" + ">"
	thinkingOpen  = "<" + "thinking" + ">"
	thinkingClose = "<" + "/" + "thinking" + ">"
	reasonOpen    = "<" + "reasoning" + ">"
	reasonClose   = "<" + "/" + "reasoning" + ">"
)

var emojiThinking = string(rune(0x1F9E0))

var reasoningOpeners = regexp.MustCompile(
	`(?i)` + regexp.QuoteMeta(thinkOpen) +
		`|` + regexp.QuoteMeta(thinkingOpen) +
		`|` + regexp.QuoteMeta(reasonOpen) +
		`|` + regexp.QuoteMeta(emojiThinking))

// reasoningClosers maps each opener (lowercased) to the regexp that finds its
// closing marker.
var reasoningClosers = map[string]*regexp.Regexp{
	thinkOpen:     regexp.MustCompile(`(?i)` + regexp.QuoteMeta(thinkClose)),
	thinkingOpen:  regexp.MustCompile(`(?i)` + regexp.QuoteMeta(thinkingClose)),
	reasonOpen:    regexp.MustCompile(`(?i)` + regexp.QuoteMeta(reasonClose)),
	emojiThinking: regexp.MustCompile(regexp.QuoteMeta(emojiThinking)),
}

// stripReasoningBlocks removes every reasoning block from text. It is applied
// to decoded content for every protocol so downstream consumers never see a
// chain-of-thought preamble as the model's answer.
func stripReasoningBlocks(text string) string {
	for {
		loc := reasoningOpeners.FindStringIndex(text)
		if loc == nil {
			return text
		}
		opener := strings.ToLower(text[loc[0]:loc[1]])
		closer, ok := reasoningClosers[opener]
		if !ok {
			return text
		}
		rest := text[loc[1]:]
		closing := closer.FindStringIndex(rest)
		if closing == nil || closing[1] == 0 {
			// Unclosed reasoning, or a zero-width closer that cannot advance:
			// the response was truncated inside the reasoning, so there is no
			// answer to keep.
			return text[:loc[0]]
		}
		text = text[:loc[0]] + rest[closing[1]:]
	}
}
