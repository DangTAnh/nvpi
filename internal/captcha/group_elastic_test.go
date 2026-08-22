package captcha

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeBrowser builds a Browser that Close()s safely without ever touching
// Chrome: cancel/bCancel are the only non-nil funcs Close invokes.
func fakeBrowser() *Browser {
	return &Browser{
		cancel:  func() {},
		bCancel: func() {},
	}
}

// newTestGroup builds an elastic-capable group around an injected factory —
// no Chrome processes involved.
func newTestGroup(t *testing.T, initial, max int) *BrowserGroup {
	t.Helper()
	g := &BrowserGroup{
		parent:         context.Background(),
		browserFactory: func(context.Context, BrowserConfig) (*Browser, error) { return fakeBrowser(), nil },
		browsers:       make([]*Browser, 0, initial),
		busy:           make(map[*Browser]bool),
		lastUsed:       make(map[*Browser]time.Time),
		maxSize:        max,
		idleTTL:        time.Hour, // shrink inert unless the test opts in
		notify:         make(chan struct{}),
		done:           make(chan struct{}),
	}
	for i := 0; i < initial; i++ {
		b := fakeBrowser()
		g.browsers = append(g.browsers, b)
		g.lastUsed[b] = time.Now()
		g.free = append(g.free, b)
	}
	t.Cleanup(g.Close)
	return g
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Borrow round-trip: acquired browsers come back and are reusable.
func TestGroupAcquireReleaseRoundTrip(t *testing.T) {
	g := newTestGroup(t, 1, 1)

	b, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got := g.Len(); got != 1 {
		t.Fatalf("Len after borrow = %d, want 1", got)
	}
	g.release(b)

	b2, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if b2 != b {
		t.Fatalf("expected same browser back, got different")
	}
}

// A borrow miss with headroom spawns a Chrome asynchronously, capped at max;
// beyond the cap acquire blocks until ctx expires instead of growing.
func TestGroupSpawnsUnderPressureUpToMax(t *testing.T) {
	g := newTestGroup(t, 1, 3)

	b1, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	b2, err := g.acquire(ctx2) // free list empty -> background spawn
	if err != nil {
		t.Fatalf("second acquire (should trigger spawn): %v", err)
	}
	waitFor(t, "scale-up to 2", func() bool { return g.Len() == 2 })

	ctx3, cancel3 := context.WithTimeout(context.Background(), time.Second)
	defer cancel3()
	b3, err := g.acquire(ctx3)
	if err != nil {
		t.Fatalf("third acquire: %v", err)
	}
	waitFor(t, "scale-up to 3", func() bool { return g.Len() == 3 })

	if _, err := g.acquire(newTimeoutCtx(150 * time.Millisecond)); err == nil {
		t.Fatalf("acquire past max should fail on ctx expiry, got a browser")
	}
	_ = b1
	_ = b2
	_ = b3
}

// Idle Chromes shrink LRU-first down to a floor of one.
func TestGroupShrinksIdleToFloor(t *testing.T) {
	g := newTestGroup(t, 3, 3)
	g.EnableElastic(3, 80*time.Millisecond)

	waitFor(t, "shrink to floor 1", func() bool { return g.Len() == 1 })
	if g.Len() != 1 {
		t.Fatalf("floor violated: %d chromes", g.Len())
	}

	// Survivor must still be borrowable and releasable.
	b, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire survivor: %v", err)
	}
	g.release(b)
}

// The shrinker may only close idle free-list browsers: a borrowed (mid-mint)
// Chrome survives even when maximally idle, and the floor of 1 always holds.
func TestGroupShrinkNeverKillsBusy(t *testing.T) {
	g := newTestGroup(t, 2, 2)
	g.EnableElastic(2, 80*time.Millisecond)

	busy, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Backdate everything so both are maximally idle by TTL terms.
	g.mu.Lock()
	for _, b := range g.browsers {
		g.lastUsed[b] = time.Now().Add(-time.Hour)
	}
	g.mu.Unlock()

	waitFor(t, "shrink of the idle free chrome", func() bool { return g.Len() == 1 })

	g.mu.Lock()
	survivor := g.browsers[0]
	g.mu.Unlock()
	if survivor != busy {
		t.Fatalf("shrinker killed the busy chrome: survivor=%v want=%v", survivor, busy)
	}
	g.release(busy)
}

// Without EnableElastic the group keeps its fixed size: no growth, no shrinker.
func TestGroupFixedWithoutElastic(t *testing.T) {
	g := newTestGroup(t, 2, 2)

	b1, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	b2, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	// Both busy: a third borrow must block (no elastic headroom).
	if _, err := g.acquire(newTimeoutCtx(120 * time.Millisecond)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third acquire should block on fixed-size group, got %v", err)
	}
	g.release(b1)
	g.release(b2)
}

// Double release is dropped, not double-counted into the free list.
func TestGroupReleaseDoubleDrop(t *testing.T) {
	g := newTestGroup(t, 1, 1)

	b, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	g.release(b)
	g.release(b) // must be ignored

	g.mu.Lock()
	n := len(g.free)
	g.mu.Unlock()
	if n != 1 {
		t.Fatalf("free list = %d after double release, want 1", n)
	}
}

// recycle swaps the dead Chrome for a fresh one and keeps bookkeeping intact.
func TestGroupRecycleSwapsBrowser(t *testing.T) {
	g := newTestGroup(t, 1, 1)

	old, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	nb, err := g.recycle(old)
	if err != nil {
		t.Fatalf("recycle: %v", err)
	}
	if nb == old {
		t.Fatalf("recycle returned the same browser")
	}
	if !old.closed {
		t.Fatalf("old browser not closed after recycle")
	}
	if got := g.Len(); got != 1 {
		t.Fatalf("Len after recycle = %d, want 1", got)
	}
	g.release(nb) // nb arrives still marked borrowed

	b, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire recycled browser: %v", err)
	}
	if b != nb {
		t.Fatalf("expected recycled browser back, got different")
	}
}

// Regression: recycle commits by identity, not positional index — the shrinker
// may remove lower-indexed browsers during the ~1-2s factory launch window,
// shifting positions. A stale index used to reject valid swaps and discard the
// freshly launched Chrome.
func TestGroupRecycleSurvivesShrinkDuringFactory(t *testing.T) {
	a := fakeBrowser()
	old := fakeBrowser()
	factoryStarted := make(chan struct{})
	releaseFactory := make(chan struct{})
	g := &BrowserGroup{
		parent:   context.Background(),
		browsers: []*Browser{a, old},
		busy:     map[*Browser]bool{old: true},
		lastUsed: map[*Browser]time.Time{a: time.Now().Add(-time.Hour), old: time.Now()},
		notify:   make(chan struct{}),
		done:     make(chan struct{}),
		browserFactory: func(context.Context, BrowserConfig) (*Browser, error) {
			close(factoryStarted)
			<-releaseFactory
			return fakeBrowser(), nil
		},
	}
	t.Cleanup(g.Close)

	done := make(chan error, 1)
	go func() {
		_, err := g.recycle(old)
		done <- err
	}()
	<-factoryStarted

	// Simulate the shrinker removing `a` (index 0) while the factory runs.
	g.mu.Lock()
	g.browsers = removeBrowser(g.browsers, a)
	delete(g.lastUsed, a)
	g.mu.Unlock()
	close(releaseFactory)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("recycle failed after index shift: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recycle did not return")
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.browsers) != 1 || g.browsers[0] == old {
		t.Fatalf("recycle did not swap old out after shift: n=%d", len(g.browsers))
	}
	if g.busy[old] {
		t.Fatalf("old browser still marked borrowed")
	}
}

func newTimeoutCtx(d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	go func() {
		<-ctx.Done()
		cancel()
	}()
	return ctx
}
