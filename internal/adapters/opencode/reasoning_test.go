package opencode

import (
	"strings"
	"testing"
)

func TestStripReasoningBlocks(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain answer", "plain answer"},
		{thinkOpen + "reasoning" + thinkClose + "answer", "answer"},
		{"pre " + thinkOpen + "r" + thinkClose + " post", "pre  post"},
		{thinkingOpen + "x" + thinkingClose + "answer", "answer"},
		{reasonOpen + "x" + reasonClose + "answer", "answer"},
		{emojiThinking + "reasoning" + emojiThinking + "Answer", "Answer"},
		{thinkOpen + "cut off with no close", ""},
		{"a " + thinkOpen + "one" + thinkClose + "b " + thinkOpen + "two" + thinkClose + "c", "a b c"},
		{"a " + strings.ToUpper(thinkOpen) + "mid" + strings.ToUpper(thinkClose) + "b", "a b"},
		{"a<b no marker here", "a<b no marker here"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := stripReasoningBlocks(tc.in); got != tc.want {
			t.Errorf("stripReasoningBlocks(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
