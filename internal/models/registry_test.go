package models

import "testing"

func TestLookupDefault(t *testing.T) {
	info, err := Lookup("")
	if err != nil {
		t.Fatalf("Lookup(\"\"): %v", err)
	}
	if info.Slug != "minimax-m3" {
		t.Fatalf("default slug = %q want minimax-m3", info.Slug)
	}
	if info.FunctionID != "87ea0ddc-cff1-4bca-bf8b-3bd98a35ddd0" {
		t.Fatalf("default function id = %q want the known MiniMax id", info.FunctionID)
	}
}

func TestLookupKnown(t *testing.T) {
	cases := map[string]string{
		"minimaxai/minimax-m3":              "minimax-m3",
		"deepseek-ai/deepseek-v4-pro":       "deepseek-v4-pro",
		"nvidia/nemotron-3-ultra-550b-a55b": "nemotron-3-ultra-550b-a55b",
	}
	for model, wantSlug := range cases {
		info, err := Lookup(model)
		if err != nil {
			t.Errorf("Lookup(%q): %v", model, err)
			continue
		}
		if info.Slug != wantSlug {
			t.Errorf("Lookup(%q) slug = %q want %q", model, info.Slug, wantSlug)
		}
		if info.Namespace != Namespace {
			t.Errorf("Lookup(%q) namespace = %q want %q", model, info.Namespace, Namespace)
		}
	}
}

func TestLookupUnknown(t *testing.T) {
	_, err := Lookup("no-such-org/no-such-model")
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
	uerr, ok := err.(*ErrUnknownModel)
	if !ok {
		t.Fatalf("error type = %T want *ErrUnknownModel", err)
	}
	if uerr.Model != "no-such-org/no-such-model" {
		t.Errorf("err model = %q", uerr.Model)
	}
}

func TestPredictEndpoint(t *testing.T) {
	info, _ := Lookup("minimaxai/minimax-m3")
	want := "https://api.ngc.nvidia.com/v2/predict/models/" + Namespace + "/minimax-m3"
	if got := info.PredictEndpoint(); got != want {
		t.Fatalf("PredictEndpoint() = %q want %q", got, want)
	}
}

// Registry invariants: every entry has a UUID-shaped function id and the shared
// namespace. Function ids are *usually* unique per model, but NVIDIA does alias
// some backend versions to the same NVCF function (e.g. the ising-calibration
// variants share 499210d3…). We log duplicates instead of failing so a legit
// alias is not mistaken for a scrape bug; the endpoint path (namespace/slug) is
// what actually distinguishes models, and that IS unique per registry key.
func TestRegistryInvariants(t *testing.T) {
	seen := map[string]string{} // functionID -> first model
	for model, info := range Models {
		if info.FunctionID == "" || !uuid42(info.FunctionID) {
			t.Errorf("%q: bad function id %q", model, info.FunctionID)
		}
		if info.Namespace != Namespace {
			t.Errorf("%q: namespace = %q want %q", model, info.Namespace, Namespace)
		}
		if info.Slug == "" {
			t.Errorf("%q: empty slug", model)
		}
		if prev, dup := seen[info.FunctionID]; dup {
			t.Logf("note: %q and %q share function id %q (likely an alias)", prev, model, info.FunctionID)
		} else {
			seen[info.FunctionID] = model
		}
	}

	// Slugs within the shared namespace must be unique — otherwise two models
	// would collide on the predict URL path.
	slugSeen := map[string]string{}
	for model, info := range Models {
		if prev, dup := slugSeen[info.Slug]; dup {
			t.Errorf("duplicate slug %q for %q and %q (predict URL collision)", info.Slug, prev, model)
		} else {
			slugSeen[info.Slug] = model
		}
	}
}

func uuid42(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return false
			}
		}
	}
	return true
}

// ContextLength values scraped from the playground pages'
// `\"contextLength\":<int>` literal. The crawler no longer parses that field
// (NVIDIA dropped the inline literal), so these pinned values are historical
// — pages that did not inline the field are absent here and
// must stay 0 — callers apply their own default (see ModelInfo.ContextLength).
func TestContextLengths(t *testing.T) {
	resolved := map[string]int{
		"minimaxai/minimax-m3":                    1048576,
		"minimaxai/minimax-m2.7":                  204800,
		"deepseek-ai/deepseek-v4-pro":             1048576,
		"deepseek-ai/deepseek-v4-flash":           1048576,
		"nvidia/nemotron-3-ultra-550b-a55b":       1048576,
		"nvidia/nemotron-3-super-120b-a12b":       1048576,
		"meta/llama-4-maverick-17b-128e-instruct": 1048576,
		"openai/gpt-oss-120b":                     131072,
		"bytedance/seed-oss-36b-instruct":         524288,
		"google/gemma-3n-e4b-it":                  32768,
		"mistralai/mixtral-8x7b-instruct":         32768,
		"nvidia/nemotron-mini-4b-instruct":        4096,
	}
	for model, want := range resolved {
		info, err := Lookup(model)
		if err != nil {
			t.Errorf("Lookup(%q): %v", model, err)
			continue
		}
		if info.ContextLength != want {
			t.Errorf("Lookup(%q) ContextLength = %d want %d", model, info.ContextLength, want)
		}
	}

	// The default model must carry its real (well above the 262144 fallback)
	// context length — this is what stops premature context compaction.
	if info, _ := Lookup("minimaxai/minimax-m3"); info.ContextLength <= 262144 {
		t.Errorf("minimax-m3 ContextLength = %d, want > 262144 (fallback)", info.ContextLength)
	}

	unresolved := []string{
		"mistralai/mistral-nemotron",
	}
	for _, model := range unresolved {
		info, err := Lookup(model)
		if err != nil {
			t.Errorf("Lookup(%q): %v", model, err)
			continue
		}
		if info.ContextLength != 0 {
			t.Errorf("Lookup(%q) ContextLength = %d, want 0 (unresolved scrape)", model, info.ContextLength)
		}
	}

	// No entry may carry a garbage scrape: 0 (unresolved) or a real value.
	for model, info := range Models {
		if info.ContextLength != 0 && info.ContextLength < 4096 {
			t.Errorf("%q: suspicious ContextLength %d (want 0 or >= 4096)", model, info.ContextLength)
		}
	}
}
