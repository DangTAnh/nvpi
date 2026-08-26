package models

import (
	"context"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

// TestLivePlaygroundProbe hits build.nvidia.com for every hardcoded registry
// entry through the real probePlayground path (concurrency 4, same retry rule
// as Refresh). Skipped unless SIM=1 — it is a live network sweep, not a unit
// test. Use it to sanity-check the keep/drop signal or to hunt for retired
// entries to prune from registry.go:
//
//	SIM=1 go test -run TestLivePlaygroundProbe -v ./internal/models/
func TestLivePlaygroundProbe(t *testing.T) {
	if os.Getenv("SIM") == "" {
		t.Skip("live probe — set SIM=1 to run")
	}
	hc := &http.Client{Timeout: 15 * time.Second}
	ids := make([]string, 0, len(Models))
	for id := range Models {
		ids = append(ids, id)
	}
	var mu sync.Mutex
	var dropped []string
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	start := time.Now()
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if !probePlayground(context.Background(), hc, id) {
				mu.Lock()
				dropped = append(dropped, id)
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
	t.Logf("dropped %d/%d in %s: %v", len(dropped), len(ids), time.Since(start).Round(time.Second), dropped)
}

// TestLiveRefresh runs the real Refresh pipeline with cmd/serve's defaults
// (30s total budget, concurrency 8, 15s client) and reports the result plus
// whether SIM_ID (default nvidia/nemotron-3.5-lightning-30b-a3b) survived:
//
//	SIM=1 go test -run TestLiveRefresh -v ./internal/models/
func TestLiveRefresh(t *testing.T) {
	if os.Getenv("SIM") == "" {
		t.Skip("live refresh — set SIM=1 to run")
	}
	id := os.Getenv("SIM_ID")
	if id == "" {
		id = "nvidia/nemotron-3.5-lightning-30b-a3b"
	}
	res, err := Refresh(context.Background(), RefreshOptions{})
	t.Logf("refresh result: %+v err=%v", res, err)
	if _, err := Lookup(id); err == nil {
		t.Logf("%s: KEPT", id)
	} else {
		t.Logf("%s: DROPPED", id)
	}
}
