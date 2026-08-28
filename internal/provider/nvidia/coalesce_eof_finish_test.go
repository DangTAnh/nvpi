package nvidia

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestCoalesceSSE_InjectsFinishOnEOF documents the kimi-k3 mid-stream
// disconnect fix: when upstream closes the connection without emitting
// a finish_reason chunk, the gateway must synthesize one so the SDK
// emits Anthropic message_delta + message_stop, otherwise Claude Code
// sees the assistant turn as still-streaming and tools stop running.
func TestCoalesceSSE_InjectsFinishOnEOF(t *testing.T) {
	// Three content deltas then EOF, no finish_reason.
	upstream := strings.Join([]string{
		`data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"Đọc"}}]}`,
		``,
		`data: {"id":"x","choices":[{"index":0,"delta":{"content":" file"}}]}`,
		``,
		`data: {"id":"x","choices":[{"index":0,"delta":{"content":" game.py"}}]}`,
		``,
	}, "\n")

	var events []string
	if err := coalesceSSEEvents(strings.NewReader(upstream), 50*time.Millisecond, func(line string) error {
		events = append(events, strings.TrimPrefix(line, "data: "))
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if len(events) == 0 {
		t.Fatal("expected at least one emitted event")
	}
	last := events[len(events)-1]
	if last == "[DONE]" {
		t.Fatalf("upstream had no [DONE] but coalesce emitted one; last=%s", last)
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(last), &chunk); err != nil {
		t.Fatalf("last event not JSON: %s (err=%v)", last, err)
	}
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("synthetic finish chunk missing choices: %s", last)
	}
	c0 := choices[0].(map[string]any)
	if c0["finish_reason"] != "stop" {
		t.Fatalf("expected synthetic finish_reason=stop, got %v", c0["finish_reason"])
	}
}

// TestCoalesceSSE_NoInjectWhenDONE: when upstream properly closes with
// [DONE], the synthetic finish must NOT be emitted (avoids duplicate).
func TestCoalesceSSE_NoInjectWhenDONE(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var events []string
	if err := coalesceSSEEvents(strings.NewReader(upstream), 50*time.Millisecond, func(line string) error {
		events = append(events, strings.TrimPrefix(line, "data: "))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[len(events)-1] != "[DONE]" {
		t.Fatalf("last event should be [DONE], got %v", events)
	}
}

// TestCoalesceSSE_NoInjectWhenFinishAlreadySeen: upstream emitted
// finish_reason, so the synthetic must not duplicate it.
func TestCoalesceSSE_NoInjectWhenFinishAlreadySeen(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
		``,
		`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var events []string
	if err := coalesceSSEEvents(strings.NewReader(upstream), 50*time.Millisecond, func(line string) error {
		events = append(events, strings.TrimPrefix(line, "data: "))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e == "[DONE]" {
			continue
		}
		var c map[string]any
		if err := json.Unmarshal([]byte(e), &c); err != nil {
			continue
		}
		if arr, ok := c["choices"].([]any); ok && len(arr) > 0 {
			if c0, ok := arr[0].(map[string]any); ok {
				if fr, ok := c0["finish_reason"].(string); ok && fr != "" {
					count := 0
					for _, x := range events {
						if strings.Contains(x, `"finish_reason":"`+fr+`"`) {
							count++
						}
					}
					if count != 1 {
						t.Fatalf("expected exactly 1 finish_reason=%s, got %d (events=%v)", fr, count, events)
					}
				}
			}
		}
	}
}

// TestCoalesceSSE_NoInjectOnEmptyStream: no choices seen at all → no
// synthetic finish (avoids fabricating a phantom turn).
func TestCoalesceSSE_NoInjectOnEmptyStream(t *testing.T) {
	var events []string
	if err := coalesceSSEEvents(strings.NewReader(""), 50*time.Millisecond, func(line string) error {
		events = append(events, line)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("expected no events on empty stream, got %v", events)
	}
}
