package nvidia

import (
	"encoding/json"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"

	_ "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
)

func TestTranslateToChat_preservesResponsesCompatibleFields(t *testing.T) {
	// Given: a Responses request containing fields that Chat Completions can express.
	request := []byte(`{
		"model":"minimaxai/minimax-m3",
		"input":"hello",
		"temperature":0.25,
		"top_p":0.8,
		"user":"user-42",
		"service_tier":"default",
		"text":{"format":{"type":"json_schema","name":"answer","strict":true,"schema":{"type":"object"}}}
	}`)

	// When: the request is translated to the canonical Chat format.
	got, err := translateToChat(sdktranslator.FormatOpenAIResponse, "minimaxai/minimax-m3", request, false)
	if err != nil {
		t.Fatal(err)
	}

	// Then: compatible scalar fields and structured output retain their machine-readable shape.
	var body map[string]json.RawMessage
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"temperature", "top_p", "user", "service_tier"} {
		if _, ok := body[field]; !ok {
			t.Errorf("translated request lost %q: %s", field, got)
		}
	}
	var responseFormat struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name   string          `json:"name"`
			Strict bool            `json:"strict"`
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(body["response_format"], &responseFormat); err != nil {
		t.Fatal(err)
	}
	if responseFormat.Type != "json_schema" || responseFormat.JSONSchema.Name != "answer" ||
		!responseFormat.JSONSchema.Strict || len(responseFormat.JSONSchema.Schema) == 0 {
		t.Fatalf("response_format=%s", body["response_format"])
	}
}

func TestTranslateToChat_preservesClaudeTemperatureAndTopP(t *testing.T) {
	// Given: Claude parameters whose translator currently treats as mutually exclusive.
	request := []byte(`{
		"model":"minimaxai/minimax-m3",
		"max_tokens":32,
		"temperature":0.2,
		"top_p":0.7,
		"messages":[{"role":"user","content":"hello"}]
	}`)

	// When: the request is translated to canonical Chat.
	got, err := translateToChat(sdktranslator.FormatClaude, "minimaxai/minimax-m3", request, false)
	if err != nil {
		t.Fatal(err)
	}

	// Then: both independent sampling controls are present.
	var body map[string]json.RawMessage
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatal(err)
	}
	if string(body["temperature"]) != "0.2" || string(body["top_p"]) != "0.7" {
		t.Fatalf("sampling fields were not preserved: %s", got)
	}
}

func TestTranslateToChat_ignoresUnsupportedPlatformFeatures(t *testing.T) {
	tests := []struct {
		name    string
		format  sdktranslator.Format
		request string
	}{
		{
			name:    "responses store",
			format:  sdktranslator.FormatOpenAIResponse,
			request: `{"model":"minimaxai/minimax-m3","input":"hello","store":true}`,
		},
		{
			name:    "responses state",
			format:  sdktranslator.FormatOpenAIResponse,
			request: `{"model":"minimaxai/minimax-m3","input":"hello","previous_response_id":"resp_1"}`,
		},
		{
			name:    "responses hosted tool",
			format:  sdktranslator.FormatOpenAIResponse,
			request: `{"model":"minimaxai/minimax-m3","input":"hello","tools":[{"type":"web_search_preview"}]}`,
		},
		{
			name:    "responses input file",
			format:  sdktranslator.FormatOpenAIResponse,
			request: `{"model":"minimaxai/minimax-m3","input":[{"role":"user","content":[{"type":"input_file","file_id":"file_1"}]}]}`,
		},
		{
			name:    "claude document",
			format:  sdktranslator.FormatClaude,
			request: `{"model":"minimaxai/minimax-m3","max_tokens":32,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","data":"AA=="}}]}]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Given: a source request that includes platform-only fields.
			// When: it is translated to Chat Completions.
			got, err := translateToChat(test.format, "minimaxai/minimax-m3", []byte(test.request), false)

			// Then: translation succeeds; unsupported fields are dropped by translation.
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) == 0 {
				t.Fatal("empty translated body")
			}
		})
	}
}

// TestTranslateToChat_CopiesClaudeExtras pins the Anthropic→OpenAI scalar
// remap that lives outside the SDK translator: top_k, stop_sequences → stop,
// metadata.user_id → user. Without these, Claude Code's sampling controls and
// user-id telemetry are silently dropped — and the upstream model picks its
// own defaults, producing output that drifts in ways the caller perceives as
// hallucination. Regression guard: if someone refactors translateToChat to
// rely solely on the SDK translator for Claude, this test breaks.
func TestTranslateToChat_CopiesClaudeExtras(t *testing.T) {
	request := []byte(`{
		"model":"minimaxai/minimax-m3",
		"max_tokens":64,
		"messages":[{"role":"user","content":"hi"}],
		"top_k":5,
		"stop_sequences":["\n\n","END"],
		"metadata":{"user_id":"u-42","team":"core"}
	}`)

	got, err := translateToChat(sdktranslator.FormatClaude, "minimaxai/minimax-m3", request, false)
	if err != nil {
		t.Fatal(err)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatal(err)
	}
	if string(body["top_k"]) != "5" {
		t.Fatalf("top_k missing or wrong: %s", body["top_k"])
	}
	if string(body["stop"]) == "" || string(body["stop"]) == "null" {
		t.Fatalf("stop_sequences → stop missing: %s", body["stop"])
	}
	// stop must be an array containing the two sequence strings.
	var stopList []string
	if err := json.Unmarshal(body["stop"], &stopList); err != nil {
		t.Fatalf("stop not an array: %v body=%s", err, body["stop"])
	}
	if len(stopList) != 2 || stopList[0] != "\n\n" || stopList[1] != "END" {
		t.Fatalf("stop=%v want [\"\\n\\n\" \"END\"]", stopList)
	}
	if string(body["user"]) != `"u-42"` {
		t.Fatalf("metadata.user_id → user missing or wrong: %s", body["user"])
	}
	// Sanity: temperature/top_p still forwarded (covered by sibling test).
	if _, ok := body["messages"]; !ok {
		t.Fatalf("messages missing — SDK translator regression: %s", got)
	}
}
