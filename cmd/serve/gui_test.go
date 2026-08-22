package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"glm52-nvidia/internal/models"

	"github.com/gin-gonic/gin"
)

// webTestEngine builds a fresh gin engine wired only with the dashboard —
// handlers receive deps via WebDeps, no global engine state.
func webTestEngine(d WebDeps) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	registerWeb(r, d)
	return r
}

func TestStatusAutoOff(t *testing.T) {
	d := WebDeps{
		Version:     "test-ver",
		Start:       time.Now().Add(-90 * time.Second),
		Auto:        false, // pool + browser stay nil
		ChromesMax:  3,
		MaxInflight: 8,
		CoalesceMs:  16,
		PoolSize:    3,
		PoolBatch:   3,
	}
	w := httptest.NewRecorder()
	webTestEngine(d).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	var s struct {
		Version       string `json:"version"`
		UptimeSeconds int64  `json:"uptime_seconds"`
		Pool          struct {
			Enabled     bool   `json:"enabled"`
			Ready       int    `json:"ready"`
			Fills       uint64 `json:"fills"`
			Takes       uint64 `json:"takes"`
			Errors      uint64 `json:"errors"`
			Expired     uint64 `json:"expired"`
			StaleLeases uint64 `json:"stale_leases"`
			TTLms       int64  `json:"ttl_ms"`
		} `json:"pool"`
		Chromes struct {
			Live int `json:"live"`
			Max  int `json:"max"`
		} `json:"chromes"`
		Limits struct {
			MaxInflight int `json:"max_inflight"`
			CoalesceMs  int `json:"coalesce_ms"`
			PoolSize    int `json:"pool_size"`
			PoolBatch   int `json:"pool_batch"`
		} `json:"limits"`
		Models int `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, w.Body.String())
	}
	if s.Version != "test-ver" {
		t.Errorf("version = %q, want %q", s.Version, "test-ver")
	}
	if s.UptimeSeconds < 89 || s.UptimeSeconds > 95 {
		t.Errorf("uptime_seconds = %d, want ~90", s.UptimeSeconds)
	}
	if s.Pool.Enabled {
		t.Error("pool.enabled = true with -auto off; want honest false")
	}
	if s.Pool.Ready != 0 || s.Pool.Fills != 0 || s.Pool.Takes != 0 ||
		s.Pool.Errors != 0 || s.Pool.Expired != 0 || s.Pool.StaleLeases != 0 || s.Pool.TTLms != 0 {
		t.Errorf("pool counters not zero when -auto off: %+v", s.Pool)
	}
	if s.Chromes.Live != 0 || s.Chromes.Max != 3 {
		t.Errorf("chromes = %+v, want live=0 max=3", s.Chromes)
	}
	if s.Limits.MaxInflight != 8 || s.Limits.CoalesceMs != 16 ||
		s.Limits.PoolSize != 3 || s.Limits.PoolBatch != 3 {
		t.Errorf("limits = %+v, want inflight=8 coalesce=16 size=3 batch=3", s.Limits)
	}
	if s.Models != len(models.Models) {
		t.Errorf("models count = %d, want %d (registry size)", s.Models, len(models.Models))
	}
}

func TestModelsSortedWithMinimax(t *testing.T) {
	w := httptest.NewRecorder()
	webTestEngine(WebDeps{}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	var got []struct {
		ID            string `json:"id"`
		ContextLength int    `json:"context_length"`
		Reasoning     bool   `json:"reasoning"`
		ToolCalling   bool   `json:"tool_calling"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, w.Body.String())
	}
	if len(got) != len(models.Models) {
		t.Fatalf("got %d models, want %d", len(got), len(models.Models))
	}

	ids := make([]string, len(got))
	for i, m := range got {
		ids[i] = m.ID
	}
	if !sort.StringsAreSorted(ids) {
		t.Errorf("models not sorted ascending by id: first=%q last=%q", ids[0], ids[len(ids)-1])
	}

	var found bool
	for _, m := range got {
		if m.ID == "minimaxai/minimax-m3" {
			found = true
			if m.ContextLength != 1048576 {
				t.Errorf("minimaxai/minimax-m3 context_length = %d, want 1048576", m.ContextLength)
			}
		}
	}
	if !found {
		t.Error("minimaxai/minimax-m3 missing from /api/models")
	}
}

func TestIndexServesHTML(t *testing.T) {
	w := httptest.NewRecorder()
	webTestEngine(WebDeps{}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}
	body := w.Body.String()
	if len(body) < 15 || body[:15] != "<!doctype html>" {
		t.Error("/ did not serve the embedded index.html doctype")
	}
	for _, marker := range []string{"NVPI", "/api/status", "/v1/chat/completions"} {
		if !strings.Contains(body, marker) {
			t.Errorf("index.html missing marker %q", marker)
		}
	}
}
