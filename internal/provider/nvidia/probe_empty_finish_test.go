package nvidia

import (
	"context"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// TestProbeSDK_DropsToolTurnWithoutNameID docs the upstream SDK bug:
// when an OpenAI stream carries tool_calls[id+name+args] in a single
// delta and the next chunk is finish_reason=stop, the SDK emits no
// content_block_start for the tool_use. The Anthropic stream that
// reaches Claude Code therefore has `content: []` and Claude Code
// surfaces "(no content)" for that turn.
//
// The gateway's emptyGuard (TestExecuteStream_EmptyToolTurnEmitsPlaceholder)
// compensates by injecting a placeholder text block before message_stop.
// If you upgrade the SDK and this probe starts passing (i.e. the SDK
// itself emits a content_block_start), you can delete the guard.
func TestProbeSDK_DropsToolTurnWithoutNameID(t *testing.T) {
	chunks := []string{
		`data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"ls\"}"}}]}}]}`,
		`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	var param any
	ctx := context.Background()
	origReq := []byte(`{"model":"minimaxai/minimax-m3","stream":true,"messages":[{"role":"user","content":"list files"}]}`)
	body := []byte(`{"stream":true}`)

	starts := 0
	for _, line := range chunks {
		out := sdktranslator.TranslateStream(
			ctx,
			sdktranslator.FormatOpenAI,
			sdktranslator.FormatClaude,
			"minimaxai/minimax-m3",
			origReq,
			body,
			[]byte(line),
			&param,
		)
		for _, c := range out {
			if strings.Contains(string(c), `"content_block_start"`) {
				starts++
			}
		}
	}
	if starts != 0 {
		t.Logf("SDK now emits %d content_block_start events for this turn — emptyGuard may be obsolete", starts)
		return
	}
	t.Log("BUG CONFIRMED: SDK drops this turn; emptyGuard compensates")
}

// TestProbeSDK_DropsEmptyContentTurn docs a second variant: an
// OpenAI stream where content="", finish_reason="stop" without any
// reasoning/tool_calls. Same downstream symptom.
func TestProbeSDK_DropsEmptyContentTurn(t *testing.T) {
	chunks := []string{
		`data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`data: {"id":"x","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	var param any
	ctx := context.Background()
	origReq := []byte(`{"model":"minimaxai/minimax-m3","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := []byte(`{"stream":true}`)

	starts := 0
	for _, line := range chunks {
		out := sdktranslator.TranslateStream(
			ctx, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude,
			"minimaxai/minimax-m3", origReq, body, []byte(line), &param)
		for _, c := range out {
			if strings.Contains(string(c), `"content_block_start"`) {
				starts++
			}
		}
	}
	if starts != 0 {
		t.Logf("SDK now emits %d content_block_start events — guard may be obsolete", starts)
		return
	}
	t.Log("BUG CONFIRMED: SDK drops empty-content turn; emptyGuard compensates")
}
