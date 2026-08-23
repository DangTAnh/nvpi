package models

import (
	"reflect"
	"testing"
)

// TestProbeRegexes pins both regexes to real playground HTML fragments.
// If either pattern drifts from what NVIDIA inlines, /v1/models silently
// shows the fallback context length and Claude Code overruns the model.
func TestProbeRegexes(t *testing.T) {
	const body = `...,\"specifications\":{\"contextLength\":1048576,\"parameters\":427040140160,\"inputModalities\":[\"Text\",\"Image\",\"Video\"],\"outputModalities\":[\"Text\"]},\"modelCapability\":{\"functionCalling\":true,\"structuredOutput\":true,\"reasoning\":false}}...`

	if m := contextLengthRE.FindSubmatch([]byte(body)); m == nil {
		t.Fatal("contextLengthRE did not match")
	} else if got := string(m[1]); got != "1048576" {
		t.Fatalf("contextLengthRE captured %q, want 1048576", got)
	}

	if m := capabilityJSONRE.FindSubmatch([]byte(body)); m == nil {
		t.Fatal("capabilityJSONRE did not match")
	} else {
		want := map[string]string{"1": "true", "2": "true", "3": "false"}
		for i, k := range []string{"1", "2", "3"} {
			if string(m[i+1]) != want[k] {
				t.Fatalf("capabilityJSONRE m[%d] = %q, want %q", i+1, string(m[i+1]), want[k])
			}
		}
	}

	if m := outputModalitiesRE.FindSubmatch([]byte(body)); m == nil {
		t.Fatal("outputModalitiesRE did not match")
	} else if got := string(m[1]); got != `\"Text\"` {
		t.Fatalf("outputModalitiesRE captured %q, want escaped \"Text\"", got)
	}
}

// TestParseOutputModalities pins the keep/drop rule: drop only when the field
// exists AND lacks "Text"; absence of the field (or a malformed page) keeps.
func TestParseOutputModalities(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		found   bool
		hasText bool
	}{
		{
			name:    "text output keeps",
			body:    `\"inputModalities\":[\"Text\",\"Image\"],\"outputModalities\":[\"Text\"]`,
			found:   true,
			hasText: true,
		},
		{
			name:    "image-only output drops",
			body:    `\"outputModalities\":[\"Image\"]`,
			found:   true,
			hasText: false,
		},
		{
			name:    "empty array drops",
			body:    `\"outputModalities\":[]`,
			found:   true,
			hasText: false,
		},
		{
			name:    "field absent keeps (no signal)",
			body:    `\"specifications\":{\"contextLength\":4096}`,
			found:   false,
			hasText: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotFound, gotHasText := parseOutputModalities([]byte(tt.body))
			if gotFound != tt.found || gotHasText != tt.hasText {
				t.Fatalf("parseOutputModalities(%q) = (%v,%v), want (%v,%v)",
					tt.body, gotFound, gotHasText, tt.found, tt.hasText)
			}
		})
	}
}

// TestMergeRegistry pins the three merge buckets of Refresh: fresh probe data
// wins, a transient probe failure carries over the hardcoded entry, and models
// dropped from the catalog are retired.
func TestMergeRegistry(t *testing.T) {
	fresh := ModelInfo{Slug: "b", Namespace: Namespace, FunctionID: "fresh-fn"}
	hardcoded := ModelInfo{Slug: "b", Namespace: Namespace, FunctionID: "old-fn"}

	tests := []struct {
		name       string
		discovered map[string]ModelInfo
		catalogIDs []string
		current    map[string]ModelInfo
		want       map[string]ModelInfo
	}{
		{
			name:       "probed OK uses fresh data over current",
			discovered: map[string]ModelInfo{"a/b": fresh},
			catalogIDs: []string{"a/b"},
			current:    map[string]ModelInfo{"a/b": hardcoded},
			want:       map[string]ModelInfo{"a/b": fresh},
		},
		{
			name:       "transient probe failure carries over current",
			discovered: map[string]ModelInfo{},
			catalogIDs: []string{"a/b"},
			current:    map[string]ModelInfo{"a/b": hardcoded},
			want:       map[string]ModelInfo{"a/b": hardcoded},
		},
		{
			name:       "probe failure without hardcoded entry skips",
			discovered: map[string]ModelInfo{},
			catalogIDs: []string{"new/model"},
			current:    nil,
			want:       map[string]ModelInfo{},
		},
		{
			name:       "absent from catalog is retired even if hardcoded",
			discovered: map[string]ModelInfo{},
			catalogIDs: []string{"a/b"},
			current:    map[string]ModelInfo{"a/b": hardcoded, "gone/model": {Slug: "model", Namespace: Namespace, FunctionID: "dead-fn"}},
			want:       map[string]ModelInfo{"a/b": hardcoded},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeRegistry(tt.discovered, tt.catalogIDs, tt.current)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("mergeRegistry = %+v, want %+v", got, tt.want)
			}
		})
	}
}
