package nvidia

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestEmptyGuard_NoContentBlockEmitsPlaceholder(t *testing.T) {
	g := &emptyGuard{}
	stop := []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	if !g.shouldEmitPlaceholder(stop) {
		t.Fatal("guard should request placeholder when message_stop arrives with no block")
	}
	if !bytes.Contains(emptyTextBlockChunks()[0], []byte("content_block_start")) {
		t.Fatal("placeholder payload missing content_block_start")
	}
}

func TestEmptyGuard_ContentBlockBeforeStopSuppressesPlaceholder(t *testing.T) {
	g := &emptyGuard{}
	g.observe([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
	stop := []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	if g.shouldEmitPlaceholder(stop) {
		t.Fatal("guard must suppress placeholder once any content block has been seen")
	}
}

func TestEmptyGuard_OnlyFiresOnce(t *testing.T) {
	g := &emptyGuard{}
	stop := []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	if !g.shouldEmitPlaceholder(stop) {
		t.Fatal("first call should fire")
	}
	if g.shouldEmitPlaceholder(stop) {
		t.Fatal("second call must not fire")
	}
}

func TestEmptyGuard_ResetForNextTurn(t *testing.T) {
	g := &emptyGuard{}
	g.done = true
	g.reset()
	if g.done {
		t.Fatal("reset should clear done")
	}
	stop := []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	if !g.shouldEmitPlaceholder(stop) {
		t.Fatal("after reset, a fresh empty turn must still trigger placeholder")
	}
}

func TestPatchEmptyAnthropicTurn_RepairsEmptyContent(t *testing.T) {
	in := []byte(`{"id":"x","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	out := patchEmptyAnthropicTurn(in)
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) == 0 {
		t.Fatalf("expected placeholder block injected, got content=%s", out)
	}
	if msg.Content[0].Type != "text" || msg.Content[0].Text != "." {
		t.Fatalf("placeholder shape wrong: %+v", msg.Content[0])
	}
	if msg.StopReason != "end_turn" {
		t.Fatalf("stop_reason must be preserved, got %q", msg.StopReason)
	}
}

func TestPatchEmptyAnthropicTurn_PassesThroughValidResponse(t *testing.T) {
	in := []byte(`{"id":"x","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`)
	out := patchEmptyAnthropicTurn(in)
	if !bytes.Equal(in, out) {
		t.Fatalf("expected pass-through, got diff:\nbefore: %s\nafter:  %s", in, out)
	}
}

func TestPatchEmptyAnthropicTurn_HandlesMissingContentKey(t *testing.T) {
	in := []byte(`{"id":"x","type":"message","role":"assistant","model":"m","stop_reason":"end_turn"}`)
	out := patchEmptyAnthropicTurn(in)
	var msg map[string]any
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatal(err)
	}
	content, ok := msg["content"]
	if !ok {
		t.Fatalf("content key must be added: %s", out)
	}
	arr, _ := content.([]any)
	if len(arr) == 0 {
		t.Fatalf("content must be non-empty placeholder array: %s", out)
	}
}

func TestExecuteStream_EmptyToolTurnEmitsPlaceholder(t *testing.T) {
	// Same scenario as TestProbeEmptyFinishReasonChunk: tool_call delta
	// arrives without name+id split, finish_reason=stop, [DONE]. SDK
	// emits no content_block_start; the gateway guard must inject one.
	var param any
	guard := &emptyGuard{}

	chunks := []string{
		`data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"ls\"}"}}]}}]}`,
		`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}

	origReq := []byte(`{"model":"minimaxai/minimax-m3","stream":true,"messages":[{"role":"user","content":"list files"}]}`)
	body := []byte(`{"stream":true}`)
	var emitted [][]byte
	for _, line := range chunks {
		out := sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "minimaxai/minimax-m3", origReq, body, []byte(line), &param)
		for _, c := range out {
			guard.observe(c)
			if guard.shouldEmitPlaceholder(c) {
				emitted = append(emitted, emptyTextBlockChunks()...)
			}
			emitted = append(emitted, c)
		}
	}

	hasBlock := false
	for _, e := range emitted {
		if strings.Contains(string(e), `"content_block_start"`) {
			hasBlock = true
		}
	}
	if !hasBlock {
		t.Fatalf("expected placeholder content_block_start in emitted stream, got %d chunks", len(emitted))
	}
}

func TestExecuteStream_NoGuardWhenTextEmittedNormally(t *testing.T) {
	// Sanity: when the SDK already emitted a real content_block_start,
	// the guard must NOT inject a duplicate.
	var param any
	guard := &emptyGuard{}

	chunks := []string{
		`data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
		`data: {"id":"x","choices":[{"index":0,"delta":{"content":" there"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	origReq := []byte(`{"model":"minimaxai/minimax-m3","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := []byte(`{"stream":true}`)

	placeholderStarts := 0
	for _, line := range chunks {
		out := sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "minimaxai/minimax-m3", origReq, body, []byte(line), &param)
		for _, c := range out {
			guard.observe(c)
			if guard.shouldEmitPlaceholder(c) {
				placeholderStarts += len(emptyTextBlockChunks())
			}
		}
	}
	if placeholderStarts != 0 {
		t.Fatalf("guard injected %d placeholder events on a normal text turn", placeholderStarts)
	}
}
