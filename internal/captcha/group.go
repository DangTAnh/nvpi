package captcha

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// BrowserGroup fans Extract across independent Chrome processes.
// Same-Chrome multi-tab does not work on build.nvidia.com: a second tab never
// mounts the invisible hCaptcha widget (CreateTarget probe: widget timeout).
//
// The group is elastic when EnableElastic is called: it starts at its initial
// size, spawns Chromes on borrow pressure up to the configured max, and closes
// Chromes that sit idle past the idle TTL — never below one Chrome and never
// one that is mid-mint. Each Chrome is ~150–300MB and bursty chat workloads
// leave them idle most of the time, so idle capacity is pure RAM waste.
//
// After a hard extract failure (empty token / dead widget), the offending
// Chrome is killed and replaced so the group is not stuck on a zombie process.
type BrowserGroup struct {
	parent         context.Context
	cfg            BrowserConfig
	browserFactory func(context.Context, BrowserConfig) (*Browser, error)

	mu            sync.Mutex
	browsers      []*Browser             // every live Chrome (free + busy)
	free          []*Browser             // idle, borrowable
	busy          map[*Browser]bool      // currently borrowed (mid-mint)
	lastUsed      map[*Browser]time.Time // last release (or creation) — shrink input
	spawning      int                    // in-flight elastic Chrome launches
	lastSpawnFail time.Time              // spawn failure backoff
	maxSize       int                    // spawn ceiling; = initial size until EnableElastic
	idleTTL       time.Duration
	elastic       bool
	notify        chan struct{} // closed/remade on every state change
	done          chan struct{}
	closed        bool
}

// NewBrowserGroup starts n warmed browsers (n Chrome processes). n <= 0 starts
// empty — elastic spawn-on-pressure or AppendWarmed fills it. Fixed-size
// unless EnableElastic is called afterwards.
func NewBrowserGroup(parent context.Context, n int, cfg BrowserConfig) (*BrowserGroup, error) {
	cfg = cfg.withDefaults()
	g := &BrowserGroup{
		parent:         parent,
		cfg:            cfg,
		browserFactory: NewBrowser,
		browsers:       make([]*Browser, 0, n),
		busy:           make(map[*Browser]bool),
		lastUsed:       make(map[*Browser]time.Time),
		maxSize:        n,
		notify:         make(chan struct{}),
		done:           make(chan struct{}),
	}
	if cfg.Proxy != "" {
		log.Printf("captcha chrome proxy=%s", cfg.Proxy)
	}
	for i := 0; i < n; i++ {
		b, err := g.browserFactory(parent, cfg)
		if err != nil {
			g.Close()
			return nil, fmt.Errorf("captcha browser %d: %w", i, err)
		}
		g.browsers = append(g.browsers, b)
		g.lastUsed[b] = time.Now()
		g.free = append(g.free, b)
	}
	return g, nil
}

// AppendWarmed adds an already-warmed browser to the group as free capacity —
// the playground selector hands over its probe Chrome so the pool's first mint
// skips a navigate (the winning page is already mounted). Takes ownership: a
// closed group closes b instead.
func (g *BrowserGroup) AppendWarmed(b *Browser) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		b.Close()
		return
	}
	g.browsers = append(g.browsers, b)
	g.lastUsed[b] = time.Now()
	g.free = append(g.free, b)
	log.Printf("captcha group: seeded warmed chrome (%d now live)", len(g.browsers))
	g.notifyLocked()
	g.mu.Unlock()
}

// EnableElastic lets the group grow past its initial size under borrow
// pressure (a borrow miss spawns a Chrome, up to maxChromes) and shrink by
// closing Chromes idle longer than idleTTL — never below one Chrome, never a
// borrowed one, and the survivors are the most-recently-used (warmest sticky
// tabs). Call once at startup, before Extract traffic. Without it the group
// keeps its fixed initial size. idleTTL <= 0 means the 10min sticky-tab
// staleness limit: an idle-10min widget needs a full re-navigate anyway, so
// recycling it loses nothing but RAM.
func (g *BrowserGroup) EnableElastic(maxChromes int, idleTTL time.Duration) {
	if maxChromes < 1 {
		maxChromes = 1
	}
	if idleTTL <= 0 {
		idleTTL = stickyMaxIdle
	}
	g.mu.Lock()
	if g.closed || g.elastic {
		g.mu.Unlock()
		return
	}
	g.elastic = true
	if maxChromes > g.maxSize {
		g.maxSize = maxChromes
	}
	g.idleTTL = idleTTL
	g.mu.Unlock()
	go g.shrinker()
}

// Len returns how many Chrome processes the group currently runs — the live
// number for healthz, not a configured constant.
func (g *BrowserGroup) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.browsers)
}

// acquire borrows one browser, spawning one in the background on a miss while
// under the size ceiling. Borrowers get the most-recently-used free browser so
// cold ones stay cold (and become shrink victims first).
func (g *BrowserGroup) acquire(ctx context.Context) (*Browser, error) {
	for {
		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()
			return nil, fmt.Errorf("captcha browser group closed")
		}
		if len(g.free) > 0 {
			b := g.takeMRULocked()
			g.busy[b] = true
			g.mu.Unlock()
			return b, nil
		}
		if len(g.browsers)+g.spawning < g.maxSize && time.Since(g.lastSpawnFail) > 5*time.Second {
			g.spawning++
			go g.spawnAsync()
		}
		n := g.notify
		g.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-g.done:
			return nil, fmt.Errorf("captcha browser group closed")
		case <-n:
		}
	}
}

// takeMRULocked pops the free browser with the newest lastUsed. g.mu held.
func (g *BrowserGroup) takeMRULocked() *Browser {
	best, bestAt := 0, time.Time{}
	for i, b := range g.free {
		if at := g.lastUsed[b]; at.After(bestAt) {
			best, bestAt = i, at
		}
	}
	b := g.free[best]
	g.free = append(g.free[:best], g.free[best+1:]...)
	return b
}

// spawnAsync warms one Chrome off the hot path and offers it to waiters.
func (g *BrowserGroup) spawnAsync() {
	b, err := g.browserFactory(g.parent, g.cfg)

	g.mu.Lock()
	g.spawning--
	if g.closed {
		g.mu.Unlock()
		if b != nil {
			b.Close()
		}
		return
	}
	if err != nil {
		// Back off so a broken environment cannot spin Chrome launches.
		g.lastSpawnFail = time.Now()
		g.mu.Unlock()
		log.Printf("captcha group: elastic chrome spawn failed: %v", err)
		g.mu.Lock()
		g.notifyLocked()
		g.mu.Unlock()
		return
	}
	g.browsers = append(g.browsers, b)
	g.lastUsed[b] = time.Now()
	g.free = append(g.free, b)
	log.Printf("captcha group: scaled up to %d chrome(s)", len(g.browsers))
	g.notifyLocked()
	g.mu.Unlock()
}

// shrinker closes Chromes idle past idleTTL, least-recently-used first, while
// more than one remains. Only free-list browsers are candidates, so a Chrome
// mid-mint is never killed; survivors are the recently-used warm ones whose
// sticky tabs (and rendered batch widgets) stay assets.
func (g *BrowserGroup) shrinker() {
	interval := g.idleTTL / 4
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-g.done:
			return
		case <-t.C:
		}

		g.mu.Lock()
		var victims []*Browser
		for len(g.free) > 0 && len(g.browsers) > 1 {
			victim, idleSince := g.lruFreeLocked()
			if time.Since(idleSince) < g.idleTTL {
				break
			}
			g.free = removeBrowser(g.free, victim)
			g.browsers = removeBrowser(g.browsers, victim)
			delete(g.lastUsed, victim)
			victims = append(victims, victim)
		}
		g.notifyLocked()
		g.mu.Unlock()

		for _, b := range victims {
			b.Close()
			log.Printf("captcha group: recycled idle chrome (%d now live)", g.Len())
		}
	}
}

// lruFreeLocked returns the least-recently-used free browser and its lastUsed
// time. Caller guarantees len(g.free) > 0. g.mu held.
func (g *BrowserGroup) lruFreeLocked() (*Browser, time.Time) {
	best := g.free[0]
	bestAt := g.lastUsed[best]
	for _, b := range g.free[1:] {
		if at := g.lastUsed[b]; at.Before(bestAt) {
			best, bestAt = b, at
		}
	}
	return best, bestAt
}

func removeBrowser(bs []*Browser, b *Browser) []*Browser {
	for i, x := range bs {
		if x == b {
			return append(bs[:i], bs[i+1:]...)
		}
	}
	return bs
}

// Extract borrows a free browser, mints one token, then returns it to the pool.
// Recovery is layered, mirroring BoxPwnr NimClient's reload→relaunch ladders:
// a hard failure first gets one cheap in-place retry on the *same* browser
// (the "reload" rung — a cold-start missing-captcha often clears on a second
// execute), and only a second hard failure recycles the whole Chrome process
// (the "relaunch" rung — clears a wedged renderer / stale session).
func (g *BrowserGroup) Extract(ctx context.Context) (string, error) {
	b, err := g.acquire(ctx)
	if err != nil {
		return "", err
	}

	tok, err := b.Extract(ctx)
	if err == nil {
		g.release(b)
		return tok, nil
	}
	if ctx.Err() != nil || !isHardExtractFailure(err) {
		g.release(b)
		return "", err
	}
	// "reload" rung: one cheap retry on the same browser before recycling.
	// Cold-start `missing-captcha` (the dominant transient hard failure on
	// freshly-launched Chromium) clears on a second execute; recycling
	// immediately would pay the ~1–2s Chrome relaunch for a transient blip.
	log.Printf("captcha browser hard failure; retrying on same chrome: %v", err)
	tok, err = b.Extract(ctx)
	if err == nil {
		g.release(b)
		return tok, nil
	}
	if ctx.Err() != nil || !isHardExtractFailure(err) {
		g.release(b)
		return "", err
	}
	// "relaunch" rung: twice-bitten renderer / stale session — rebuild.
	log.Printf("captcha browser hard failure again; recycling chrome: %v", err)
	nb, rerr := g.recycle(b)
	if rerr != nil {
		// old browser already closed inside recycle on success only;
		// on failure keep the slot with the old browser if still usable.
		g.release(b)
		return "", fmt.Errorf("%w; chrome recycle: %v", err, rerr)
	}
	tok, err = nb.Extract(ctx)
	g.release(nb)
	if err != nil {
		return "", fmt.Errorf("after chrome recycle: %w", err)
	}
	return tok, nil
}

// ExtractBatch borrows one free browser and mints up to n tokens in a single
// visit (parallel executes on rendered invisible widgets — see batch.go).
// Recovery ladder mirrors Extract: same-browser retry, then Chrome recycle.
// An error is returned only when zero tokens were minted; partial batches are
// returned as-is (the page is demonstrably alive, so no recycle is warranted).
func (g *BrowserGroup) ExtractBatch(ctx context.Context, n int) ([]string, error) {
	b, err := g.acquire(ctx)
	if err != nil {
		return nil, err
	}

	toks, err := b.ExtractBatch(ctx, n)
	if err == nil {
		g.release(b)
		return toks, nil
	}
	// Partial batch (some tokens minted): page is alive, return as-is.
	if len(toks) > 0 {
		g.release(b)
		return toks, nil
	}
	if ctx.Err() != nil || !isHardExtractFailure(err) {
		g.release(b)
		return nil, err
	}
	// "reload" rung: one cheap same-browser retry before paying for a
	// relaunch — mirrors the single-path rationale in Extract.
	log.Printf("captcha browser hard batch failure; retrying on same chrome: %v", err)
	toks, err = b.ExtractBatch(ctx, n)
	if err == nil {
		g.release(b)
		return toks, nil
	}
	if len(toks) > 0 {
		g.release(b)
		return toks, nil
	}
	if ctx.Err() != nil || !isHardExtractFailure(err) {
		g.release(b)
		return nil, err
	}
	// "relaunch" rung: twice-bitten — rebuild the Chrome process.
	log.Printf("captcha browser hard batch failure again; recycling chrome: %v", err)
	nb, rerr := g.recycle(b)
	if rerr != nil {
		g.release(b)
		return nil, fmt.Errorf("%w; chrome recycle: %v", err, rerr)
	}
	toks, err = nb.ExtractBatch(ctx, n)
	g.release(nb)
	if err != nil {
		return nil, fmt.Errorf("after chrome recycle: %w", err)
	}
	return toks, nil
}

func (g *BrowserGroup) release(b *Browser) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	if !g.busy[b] {
		g.mu.Unlock()
		log.Printf("captcha group: release of non-borrowed browser (double release?)")
		return
	}
	delete(g.busy, b)
	g.lastUsed[b] = time.Now()
	g.free = append(g.free, b)
	g.notifyLocked()
	g.mu.Unlock()
}

// recycle replaces old with a freshly warmed Chrome. On success old is closed
// and must not be released; the returned browser is still marked borrowed —
// the caller extracts on it and releases it afterwards.
func (g *BrowserGroup) recycle(old *Browser) (*Browser, error) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, fmt.Errorf("captcha browser group closed")
	}
	if !g.busy[old] {
		g.mu.Unlock()
		return nil, fmt.Errorf("browser not in group")
	}
	index := -1
	for i, b := range g.browsers {
		if b == old {
			index = i
			break
		}
	}
	if index < 0 {
		g.mu.Unlock()
		return nil, fmt.Errorf("browser not in group")
	}
	g.mu.Unlock()

	nb, err := g.browserFactory(g.parent, g.cfg)
	if err != nil {
		return nil, err
	}

	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		nb.Close()
		return nil, fmt.Errorf("captcha browser group closed")
	}
	// ponytail: rescan by identity instead of trusting the pre-factory index —
	// the shrinker removes idle browsers while the factory runs (~1–2s), which
	// shifts positions; a stale index rejected valid swaps and threw away the
	// freshly launched Chrome. Identity also keeps the concurrent-recycle rule
	// (first committer wins, second finds old gone) intact.
	pos := -1
	for i, b := range g.browsers {
		if b == old {
			pos = i
			break
		}
	}
	if pos < 0 {
		g.mu.Unlock()
		nb.Close()
		return nil, fmt.Errorf("browser not in group")
	}
	g.browsers[pos] = nb
	delete(g.busy, old)
	g.busy[nb] = true
	g.lastUsed[nb] = time.Now()
	g.mu.Unlock()

	old.Close()
	log.Printf("captcha browser recycled after hard extract failure")
	return nb, nil
}

// Close stops every Chrome process in the group (idle and borrowed alike —
// shutdown makes their in-flight extracts fail, which is fine).
func (g *BrowserGroup) Close() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	close(g.done)
	all := make([]*Browser, 0, len(g.browsers))
	all = append(all, g.browsers...)
	g.mu.Unlock()

	for _, b := range all {
		b.Close()
	}
}

// NavCount returns total re-navigates across all browsers in the group.
func (g *BrowserGroup) NavCount() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	var total uint64
	for _, b := range g.browsers {
		total += b.NavCount()
	}
	return total
}

// StickyCount returns total sticky executes across all browsers in the group.
func (g *BrowserGroup) StickyCount() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	var total uint64
	for _, b := range g.browsers {
		total += b.StickyCount()
	}
	return total
}

// notifyLocked wakes all borrow waiters. g.mu must be held.
func (g *BrowserGroup) notifyLocked() {
	close(g.notify)
	g.notify = make(chan struct{})
}

func isHardExtractFailure(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "empty captcha token") ||
		strings.Contains(s, "re-navigate failed") ||
		strings.Contains(s, "hcaptcha global not ready") ||
		strings.Contains(s, "chromedp navigate") ||
		strings.Contains(s, "captcha token did not refresh") ||
		strings.Contains(s, "captcha widget missing")
}
