package nvidia

import (
	"encoding/json"
	"fmt"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// Probe: what does the SDK translator do with a Claude turn that contains
// assistant tool_use followed by user tool_result? If tool_calls or
// tool_call_id get dropped/remapped, the model on the next turn sees an
// incomplete tool exchange and fills the gap with fabricated text — the
// "[Tool use interrupted]" hallucination.
func TestProbeClaudeToolResultRoundTrip(t *testing.T) {
	req := []byte(`{
		"model":"minimaxai/minimax-m3",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"list files"},
			{"role":"assistant","content":[
				{"type":"text","text":"Let me check."},
				{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"file1.go\nfile2.go"}
			]}
		]
	}`)
	out, err := translateToChat(sdktranslator.FormatClaude, "minimaxai/minimax-m3", req, false)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("PROBE tool_result round-trip:\n%s\n", out)

	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	msgs, _ := raw["messages"].([]any)
	for i, m := range msgs {
		b, _ := json.Marshal(m)
		t.Logf("msg[%d]: %s", i, b)
	}
}
