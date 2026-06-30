// harness_text_sanitize.go — strip model-format control tokens out of harness
// output before it becomes substrate-visible Content.
//
// Background (2026-06-28): the gemma-4-26b MLX build served by LM Studio is
// rendered with an OpenAI "harmony" chat template. gemma was not trained on the
// harmony special tokens, so on the turn after a tool result it emits them as
// *literal text* — and, because its tokenizer has no real token for them, in a
// corrupted form: `<|channel>thought\n<channel|>PONG` rather than the canonical
// `<|channel|>final<|message|>PONG`. Those markers must never reach Content or
// the dashboard. The clean fix is template-side (give LM Studio a gemma-native
// template); stripControlTokens is the defensive, backend-agnostic guard so the
// kernel stays robust to any harmony-templated local backend.
package engine

import (
	"regexp"
	"strings"
)

// harmonyTokenRe matches harmony channel control tokens in three shapes:
//
//   - well-formed:  <|channel|> <|message|> <|start|> <|end|> <|return|> <|"|>
//   - missing tail: <|channel>   (opens "<|", closes ">")
//   - missing head: <channel|>   (opens "<", closes "|>")
//
// The first arm matches any short pipe-delimited token (catches well-formed
// harmony/ChatML markers and the corrupted quote token `<|"|>`); the second arm
// is restricted to the harmony keyword vocabulary so ordinary `<x>`-style markup
// in real content is never disturbed.
var harmonyTokenRe = regexp.MustCompile(
	`<\|[^<>|]{0,32}\|>` +
		`|<\|?(?:start|end|message|channel|constrain|return|call|refusal)\|?>`,
)

// harmonyChannelLineRe matches a line that is nothing but a harmony channel or
// role name left dangling once its surrounding tokens were removed (e.g. the
// bare `thought` in `<|channel>thought<channel|>...`).
var harmonyChannelLineRe = regexp.MustCompile(
	`(?i)^(?:analysis|commentary|final|thought|assistant|developer|system|user)$`,
)

// stripControlTokens removes harmony/control-token noise from model output.
// Clean text (the overwhelming common case) is returned unchanged via the fast
// path — only strings that actually contain harmony delimiters are rewritten.
//
// Strategy: replace every control token with a newline so adjacent channel
// names and message bodies land on separate lines, then drop empty lines and
// dangling channel/role names. When a standalone `final` channel marker
// survives, only the text after the last one is kept (harmony places the
// user-visible answer in the `final` channel, after any `analysis`/`commentary`).
func stripControlTokens(s string) string {
	// Fast path: no harmony delimiters at all → nothing to strip.
	if !strings.Contains(s, "<|") && !strings.Contains(s, "|>") {
		return s
	}

	cleaned := harmonyTokenRe.ReplaceAllString(s, "\n")
	lines := strings.Split(cleaned, "\n")

	// If the final channel marker survived as its own line, the user-visible
	// answer is everything after the LAST one; discard preceding channels.
	lastFinal := -1
	for i, ln := range lines {
		if strings.EqualFold(strings.TrimSpace(ln), "final") {
			lastFinal = i
		}
	}
	if lastFinal >= 0 && lastFinal+1 < len(lines) {
		lines = lines[lastFinal+1:]
	}

	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" {
			continue
		}
		if harmonyChannelLineRe.MatchString(trimmed) {
			continue // dangling channel/role name, not content
		}
		kept = append(kept, trimmed)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}
