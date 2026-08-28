package nvidia

import (
	"bytes"
	"encoding/json"
	"strings"
)

// emptyGuard tracks whether the in-flight Anthropic SSE stream has
// emitted any content_block_start. When `message_stop` arrives before
// any block has been opened, the SDK has produced an empty assistant
// turn; the client (Claude Code) renders that as "(no content)".
//
// The guard is consulted by the streaming emit loop in executor.go to
// inject a single placeholder text block just before message_stop, so
// the turn is visible and the conversation history stays continuous.
//
// The non-streaming path uses patchEmptyAnthropicTurn instead.
type emptyGuard struct {
	sawContentBlock bool
	done            bool
}

// shouldEmitPlaceholder reports whether the supplied payload is the
// `message_stop` event and no content block has been opened yet.
func (g *emptyGuard) shouldEmitPlaceholder(payload []byte) bool {
	if g.done || g.sawContentBlock {
		return false
	}
	if isAnthropicMessageStop(payload) {
		g.done = true
		return true
	}
	return false
}

// observe notes any content_block_start seen on the wire so the guard
// stays accurate for future turns (each assistant turn is a separate
// stream, so the executor must reset the guard at turn boundaries).
func (g *emptyGuard) observe(payload []byte) {
	if sawAnthropicContentBlock(payload) {
		g.sawContentBlock = true
	}
}

// reset clears the guard for the next assistant turn.
func (g *emptyGuard) reset() {
	g.sawContentBlock = false
	g.done = false
}

// emptyTextBlockChunks returns the placeholder text block (start +
// delta + stop) split into three payloads, one per SSE event. The
// shape mirrors what the SDK translator emits for normal text blocks
// (compare openai_claude_response.go:198-218) so the host client
// accepts it without complaint. The content is a single "." — minimal,
// cheap, and rarely visible because Claude Code collapses whitespace-
// only text.
func emptyTextBlockChunks() [][]byte {
	return [][]byte{
		[]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"),
		[]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\".\"}}\n\n"),
		[]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"),
	}
}

// sawAnthropicContentBlock reports whether payload contains an
// Anthropic content_block_start event. SDK chunks are SSE-framed; the
// JSON tag is a stable string match.
func sawAnthropicContentBlock(payload []byte) bool {
	return bytes.Contains(payload, []byte(`"content_block_start"`))
}

// isAnthropicMessageStop reports whether payload carries the
// `message_stop` event terminator.
func isAnthropicMessageStop(payload []byte) bool {
	return bytes.Contains(payload, []byte(`"message_stop"`))
}

// patchEmptyAnthropicTurn repairs a non-stream Anthropic response whose
// assistant message has an empty content array. It injects a tiny text
// block (".", matching the streaming placeholder) so the client never
// displays an empty assistant turn. The stop_reason field is preserved.
//
// Returns the original payload unchanged when the response is already
// valid, so the common path is zero-cost.
func patchEmptyAnthropicTurn(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	placeholder := []byte(`[{"type":"text","text":"."}]`)
	idx := bytes.Index(payload, []byte(`"content":[]`))
	if idx < 0 {
		// Empty payload key absent (already repaired, or unusual shape).
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(payload, &probe); err != nil {
			return payload
		}
		if _, ok := probe["content"]; ok {
			// Non-empty content already; leave it.
			return payload
		}
		// Synthesise a content key just before the closing brace.
		end := bytes.LastIndexByte(payload, '}')
		if end < 0 {
			return payload
		}
		out := make([]byte, 0, len(payload)+len(`,"content":`)+len(placeholder))
		out = append(out, payload[:end]...)
		out = append(out, []byte(`,"content":`)...)
		out = append(out, placeholder...)
		out = append(out, payload[end:]...)
		return out
	}
	end := idx + len(`"content":`)
	out := make([]byte, 0, len(payload)+len(placeholder)-len(`[]`))
	out = append(out, payload[:end]...)
	out = append(out, placeholder...)
	out = append(out, payload[end+len(`[]`):]...)
	_ = strings.TrimSpace
	return out
}
