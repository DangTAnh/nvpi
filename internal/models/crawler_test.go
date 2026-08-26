package models

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestPlaygroundExists pins the keep/drop rule against real page shapes
// observed live on 2026-08-24: real /playground routes stream a full RSC
// payload without any error digest; missing ones answer 200 but inline the
// Next.js error digest NEXT_HTTP_ERROR_FALLBACK;404 into the payload.
func TestPlaygroundExists(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "chat page keeps (deepseek-ai/deepseek-r1)",
			body: `0:{\"P\":null,\"c\":[\"\",\"deepseek-ai\",\"deepseek-r1\",\"playground\"]...`,
			want: true,
		},
		{
			name: "soft-404 shell drops (baai/bge-m3)",
			body: `<script>self.__next_f.push([1,"20:E{\"digest\":\"NEXT_HTTP_ERROR_FALLBACK;404\"}\n"])</script>`,
			want: false,
		},
		{
			name: "soft-404 shell drops even with model metadata (nvidia/riva-asr)",
			body: `<title>riva-asr Model by NVIDIA</title>...digest\":\"NEXT_HTTP_ERROR_FALLBACK;404\"...`,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := playgroundExists([]byte(tt.body)); got != tt.want {
				t.Fatalf("playgroundExists(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

// TestKnownTextCacheRoundTrip pins the known-text disk cache: missing file
// probes everything, a saved set loads back, and a corrupt file degrades to
// "probe everything" instead of failing the refresh.
func TestKnownTextCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "known_text_models.json")

	if got := loadKnownText(p); got != nil {
		t.Fatalf("missing file: got %v, want nil", got)
	}

	saveKnownText(p, []string{"a/b", "c/d"})
	got := loadKnownText(p)
	if !got["a/b"] || !got["c/d"] {
		t.Fatalf("round-trip lost ids: %v", got)
	}

	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadKnownText(p); got != nil {
		t.Fatalf("corrupt file: got %v, want nil", got)
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
