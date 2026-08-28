package nvidia

import (
	"encoding/json"
	"testing"

	"glm52-nvidia/internal/models"
)

func TestNormalizeRequestBody(t *testing.T) {
	in := []byte(`{"stream":true,"messages":[]}`)
	out, err := NormalizeRequestBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	// NormalizeRequestBody no longer touches stream_options — that lives in
	// forceStreamFlag (the single canonical place). Asserting its absence here
	// is the regression guard: if someone re-merges the old stream_options
	// block into NormalizeRequestBody, this test breaks.
	if _, ok := raw["stream_options"]; ok {
		t.Fatalf("NormalizeRequestBody must not inject stream_options (that's forceStreamFlag's job); got %#v", raw["stream_options"])
	}
	if _, ok := raw["chat_template_kwargs"]; ok {
		t.Fatalf("thinking kwargs should not be injected without a supported model: %#v", raw)
	}
}

func TestNormalizeRequestBodyEmptyOrNullKwargs(t *testing.T) {
	cases := []string{
		`{"model":"qwen/qwen3-next-80b-a3b-instruct","stream":false,"chat_template_kwargs":{}}`,
		`{"model":"qwen/qwen3-next-80b-a3b-instruct","stream":false,"chat_template_kwargs":null}`,
	}
	for _, in := range cases {
		out, err := NormalizeRequestBody([]byte(in))
		if err != nil {
			t.Fatalf("in=%s: %v", in, err)
		}
		var raw map[string]any
		if err := json.Unmarshal(out, &raw); err != nil {
			t.Fatal(err)
		}
		if kw, ok := raw["chat_template_kwargs"].(map[string]any); ok && len(kw) != 0 {
			t.Fatalf("in=%s thinking kwargs = %#v", in, kw)
		}
	}
}

func TestNormalizeRequestBodyPreservesThinking(t *testing.T) {
	in := []byte(`{"model":"qwen/qwen3.5-397b-a17b","stream":false,"chat_template_kwargs":{"enable_thinking":false}}`)
	out, err := NormalizeRequestBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	kw := raw["chat_template_kwargs"].(map[string]any)
	if kw["enable_thinking"] != false {
		t.Fatalf("should preserve caller kwargs, got %#v", kw)
	}
}

func TestNormalizeThinkingKwargsAliases(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		want     map[string]any
		stripped []string
	}{
		{
			name: "minimax thinking enabled",
			in:   `{"model":"minimaxai/minimax-m3","stream":false,"thinking":{"type":"enabled","clear_thinking":false}}`,
			want: map[string]any{
				"thinking_mode": "enabled",
			},
			stripped: []string{"thinking"},
		},
		{
			name: "minimax thinking disabled",
			in:   `{"model":"minimaxai/minimax-m3","stream":false,"thinking":{"type":"disabled"}}`,
			want: map[string]any{
				"thinking_mode": "disabled",
			},
			stripped: []string{"thinking"},
		},
		{
			name: "top-level enable_thinking true + effort",
			in:   `{"model":"minimaxai/minimax-m3","stream":false,"enable_thinking":true,"reasoning_effort":"high"}`,
			want: map[string]any{
				"thinking_mode": "enabled",
			},
			stripped: []string{"enable_thinking", "reasoning_effort"},
		},
		{
			name: "kwargs wins over aliases",
			in:   `{"model":"minimaxai/minimax-m3","stream":false,"chat_template_kwargs":{"thinking_mode":"enabled"},"thinking":{"type":"enabled"},"enable_thinking":true}`,
			want: map[string]any{
				"thinking_mode": "enabled",
			},
			stripped: []string{"thinking", "enable_thinking"},
		},
		{
			name: "minimax none effort disables thinking",
			in:   `{"model":"minimaxai/minimax-m3","stream":false,"reasoning_effort":"none"}`,
			want: map[string]any{
				"thinking_mode": "disabled",
			},
			stripped: []string{"reasoning_effort"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := NormalizeRequestBody([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err := json.Unmarshal(out, &raw); err != nil {
				t.Fatal(err)
			}
			for _, key := range tc.stripped {
				if _, ok := raw[key]; ok {
					t.Fatalf("alias %q should be stripped, body=%#v", key, raw)
				}
			}
			kw := raw["chat_template_kwargs"].(map[string]any)
			if len(kw) != len(tc.want) {
				t.Fatalf("kwargs=%#v want %#v", kw, tc.want)
			}
			for k, v := range tc.want {
				if kw[k] != v {
					t.Fatalf("kwargs[%q]=%#v want %#v (full=%#v)", k, kw[k], v, kw)
				}
			}
		})
	}
}

func TestNormalizeRequestBodyMapsReasoningEffortByModel(t *testing.T) {
	// Per-model effort roundups for the (now mostly retired) hardcoded
	// profiles were dropped alongside those profiles — see reasoning.go.
	// Only the MiniMax-specific cases survive because MiniMax is the one
	// model with a dedicated reasoningProfile; everything else uses the
	// generic ladder tested separately.
	cases := []struct {
		name  string
		model string
		in    string
		key   string
		want  any
	}{
		{name: "minimax medium uses adaptive thinking", model: "minimaxai/minimax-m3", in: "medium", key: "thinking_mode", want: "adaptive"},
		{name: "minimax xhigh enables thinking", model: "minimaxai/minimax-m3", in: "xhigh", key: "thinking_mode", want: "enabled"},
		{name: "nemotron ultra medium enables medium_effort", model: "nvidia/nemotron-3-ultra-550b-a55b", in: "medium", key: "medium_effort", want: true},
		{name: "nemotron ultra none disables thinking", model: "nvidia/nemotron-3-ultra-550b-a55b", in: "none", key: "enable_thinking", want: false},
		{name: "nemotron super low enables low_effort", model: "nvidia/nemotron-3-super-120b-a12b", in: "low", key: "low_effort", want: true},
		{name: "nemotron super none disables thinking", model: "nvidia/nemotron-3-super-120b-a12b", in: "none", key: "enable_thinking", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"` + tc.model + `","reasoning_effort":"` + tc.in + `","stream":false}`)
			out, err := NormalizeRequestBody(body)
			if err != nil {
				t.Fatal(err)
			}

			var raw map[string]any
			if err := json.Unmarshal(out, &raw); err != nil {
				t.Fatal(err)
			}
			kw, ok := raw["chat_template_kwargs"].(map[string]any)
			if !ok {
				t.Fatalf("chat_template_kwargs missing from %s", out)
			}
			if got := kw[tc.key]; got != tc.want {
				t.Fatalf("%s=%#v want %#v (body=%s)", tc.key, got, tc.want, out)
			}
			if _, ok := raw["reasoning_effort"]; ok {
				t.Fatalf("reasoning_effort alias was not consumed: %s", out)
			}
		})
	}
}

func TestNormalizeRequestBodyDoesNotInjectThinkingIntoUnsupportedModel(t *testing.T) {
	out, err := NormalizeRequestBody([]byte(`{"model":"qwen/qwen3-next-80b-a3b-instruct","messages":[],"stream":false}`))
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["chat_template_kwargs"]; ok {
		t.Fatalf("unsupported model received thinking kwargs: %s", out)
	}
}

// TestNormalizeNemotronUltraDefaultsEnableThinking reproduces the exact bug:
// bare OpenAI-shaped requests to the Nemotron-3 hybrid MoE models were
// leaving the upstream predict endpoint without the `chat_template_kwargs`
// block the build.nvidia.com playground's server-side template always emits.
// Upstream responds 400 in that case; the fix is to inject
// `chat_template_kwargs: {enable_thinking: true}` whenever the caller did not
// pick a tier (matching the playground's `reasoning_effort || "high"` branch).
func TestNormalizeNemotronUltraDefaultsEnableThinking(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantKwargs map[string]any
	}{
		{
			name: "ultra bare request defaults to enable_thinking true",
			body: `{"model":"nvidia/nemotron-3-ultra-550b-a55b","messages":[{"role":"user","content":"hi"}],"stream":false}`,
			wantKwargs: map[string]any{"enable_thinking": true},
		},
		{
			name: "ultra explicit high effort keeps enable_thinking true with no medium flag",
			body: `{"model":"nvidia/nemotron-3-ultra-550b-a55b","reasoning_effort":"high","stream":false}`,
			wantKwargs: map[string]any{"enable_thinking": true},
		},
		{
			name: "ultra max effort collapses to enable_thinking true",
			body: `{"model":"nvidia/nemotron-3-ultra-550b-a55b","reasoning_effort":"max","stream":false}`,
			wantKwargs: map[string]any{"enable_thinking": true},
		},
		{
			name: "super bare request defaults to enable_thinking true",
			body: `{"model":"nvidia/nemotron-3-super-120b-a12b","messages":[{"role":"user","content":"hi"}],"stream":false}`,
			wantKwargs: map[string]any{"enable_thinking": true},
		},
		{
			name: "super low effort enables low_effort marker",
			body: `{"model":"nvidia/nemotron-3-super-120b-a12b","reasoning_effort":"low","stream":false}`,
			wantKwargs: map[string]any{"enable_thinking": true, "low_effort": true},
		},
		{
			name: "explicit enable_thinking false wins over effort",
			body: `{"model":"nvidia/nemotron-3-ultra-550b-a55b","reasoning_effort":"medium","enable_thinking":false,"stream":false}`,
			wantKwargs: map[string]any{"enable_thinking": false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := NormalizeRequestBody([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err := json.Unmarshal(out, &raw); err != nil {
				t.Fatal(err)
			}
			kw, ok := raw["chat_template_kwargs"].(map[string]any)
			if !ok {
				t.Fatalf("chat_template_kwargs missing from %s", out)
			}
			if len(kw) != len(tc.wantKwargs) {
				t.Fatalf("kwargs=%#v want %#v (body=%s)", kw, tc.wantKwargs, out)
			}
			for k, v := range tc.wantKwargs {
				if kw[k] != v {
					t.Fatalf("kw[%q]=%#v want %#v (full=%#v)", k, kw[k], v, kw)
				}
			}
			if _, ok := raw["reasoning_effort"]; ok {
				t.Fatalf("reasoning_effort alias was not consumed: %s", out)
			}
		})
	}
}

func TestReasoningProfilesReferenceRegisteredModels(t *testing.T) {
	for model := range reasoningProfiles {
		if _, err := models.Lookup(model); err != nil {
			t.Errorf("reasoning profile references unsupported model %q: %v", model, err)
		}
	}
}

// injectReasoningModel registers a synthetic reasoning-capable registry entry
// so the generic effort-profile path is testable without a network refresh;
// removed again on test exit.
func injectReasoningModel(t *testing.T) string {
	const id = "test/reasoner"
	t.Cleanup(func() { delete(models.Models, id) })
	models.Models[id] = models.ModelInfo{
		Slug:       "reasoner",
		Namespace:  models.Namespace,
		FunctionID: "00000000-0000-0000-0000-000000000000",
		Capability: &models.ModelCapability{Reasoning: true},
	}
	return id
}

func TestEffortFromBudgetBoundaries(t *testing.T) {
	cases := []struct {
		budget int
		want   string
	}{
		{-1, "none"},
		{0, "none"},
		{1, "low"},
		{2047, "low"},
		{2048, "medium"},
		{8191, "medium"},
		{8192, "high"},
		{16383, "high"},
		{16384, "xhigh"},
		{32767, "xhigh"},
		{32768, "max"},
		{131072, "max"},
	}
	for _, tc := range cases {
		if got := effortFromBudget(tc.budget); got != tc.want {
			t.Errorf("effortFromBudget(%d)=%q want %q", tc.budget, got, tc.want)
		}
	}
}

func TestNormalizeThinkingBudget(t *testing.T) {
	reasoner := injectReasoningModel(t)
	cases := []struct {
		name     string
		in       string
		want     map[string]any
		stripped []string
	}{
		{
			name:     "budget only derives low",
			in:       `{"model":"` + reasoner + `","stream":false,"thinking":{"type":"enabled","budget_tokens":1024}}`,
			want:     map[string]any{"reasoning_effort": "low"},
			stripped: []string{"thinking", "thinking_budget"},
		},
		{
			name:     "top-level thinking_budget alias",
			in:       `{"model":"` + reasoner + `","stream":false,"thinking_budget":5000}`,
			want:     map[string]any{"reasoning_effort": "medium"},
			stripped: []string{"thinking", "thinking_budget"},
		},
		{
			name:     "explicit effort wins over budget",
			in:       `{"model":"` + reasoner + `","stream":false,"reasoning_effort":"high","thinking":{"budget_tokens":1024}}`,
			want:     map[string]any{"reasoning_effort": "high"},
			stripped: []string{"thinking", "reasoning_effort"},
		},
		{
			name:     "explicit disabled beats budget",
			in:       `{"model":"` + reasoner + `","stream":false,"thinking":{"type":"disabled","budget_tokens":32000}}`,
			want:     map[string]any{"reasoning_effort": "none"},
			stripped: []string{"thinking", "thinking_budget"},
		},
		{
			name:     "zero budget disables",
			in:       `{"model":"` + reasoner + `","stream":false,"thinking":{"type":"enabled","budget_tokens":0}}`,
			want:     map[string]any{"reasoning_effort": "none"},
			stripped: []string{"thinking", "thinking_budget"},
		},
		{
			name:     "minimax medium budget stays adaptive",
			in:       `{"model":"minimaxai/minimax-m3","stream":false,"thinking":{"type":"enabled","budget_tokens":4096}}`,
			want:     map[string]any{"thinking_mode": "adaptive"},
			stripped: []string{"thinking", "thinking_budget"},
		},
		{
			name:     "minimax large budget enables",
			in:       `{"model":"minimaxai/minimax-m3","stream":false,"thinking":{"type":"enabled","budget_tokens":16384}}`,
			want:     map[string]any{"thinking_mode": "enabled"},
			stripped: []string{"thinking", "thinking_budget"},
		},
		{
			name:     "minimax disabled plus budget stays off",
			in:       `{"model":"minimaxai/minimax-m3","stream":false,"thinking":{"type":"disabled","budget_tokens":32000}}`,
			want:     map[string]any{"thinking_mode": "disabled"},
			stripped: []string{"thinking", "thinking_budget"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := NormalizeRequestBody([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err := json.Unmarshal(out, &raw); err != nil {
				t.Fatal(err)
			}
			for _, key := range tc.stripped {
				if _, ok := raw[key]; ok {
					t.Fatalf("alias %q should be stripped, body=%s", key, out)
				}
			}
			kw, ok := raw["chat_template_kwargs"].(map[string]any)
			if !ok {
				t.Fatalf("chat_template_kwargs missing from %s", out)
			}
			if len(kw) != len(tc.want) {
				t.Fatalf("kwargs=%#v want %#v (body=%s)", kw, tc.want, out)
			}
			for k, v := range tc.want {
				if kw[k] != v {
					t.Fatalf("kw[%q]=%#v want %#v (body=%s)", k, kw[k], v, out)
				}
			}
		})
	}
}
