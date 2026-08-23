package captcha

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/chromedp"
)

// Browser owns one long-lived headless Chrome and one sticky playground tab.
//
// chromedp allocates the OS Chrome process on the first Run and ties it to that
// context (exec.CommandContext). Canceling a timeout used for that first Run
// kills Chrome — so we keep browserCtx alive until Close.
//
// After the playground is warm, Extract fires hcaptcha.execute({async:true})
// and polls the response (~300ms steady-state) instead of a full Navigate
// (~6–10s). After stickyMaxIdle without a successful extract, the next Extract
// re-navigates instead of burning the sticky timeout on a stale widget.
//
// For parallel pool fills use NewBrowserGroup (separate Chrome processes):
// a second tab in the same Chrome never mounts the hCaptcha widget on this site.
type Browser struct {
	browser context.Context
	cancel  context.CancelFunc // allocator
	bCancel context.CancelFunc // browser tab / process owner

	mu          sync.Mutex
	closed      bool
	warmed      bool
	lastOK      time.Time
	pgURL       string // resolved playground URL
	navCount    atomic.Uint64
	stickyCount atomic.Uint64

	// Batch-mint state (see batch.go): sitekey scraped once per page load and
	// the ids of extra invisible widgets rendered on top of NVIDIA's primary.
	// Guarded by mu. Reset on every re-navigate — extras don't survive it.
	sitekey  string
	extraIDs []string
}

// stickyMaxIdle is how long a warm playground tab is trusted for sticky execute.
// Tuned for chat workloads (e.g. Claude Code) where reasoning turns regularly
// pause minutes between requests: 60s forced a full re-navigate (~6–10s) after
// any pause, spiking TTFT. 10min keeps the sticky ~300ms path across normal
// think-gaps while still recovering a stale widget before it stops minting.
const stickyMaxIdle = 10 * time.Minute

// reNavTimeout bounds a sticky re-navigate (playground already loaded once, so
// a healthy reload is <10s with assets blocked). 90s was a dud: when NVIDIA
// rate-limits or the network blips, the worker sat 90s per failed mint and the
// pool stayed empty that whole window. 30s fails fast so a transient clears
// sooner and a hard block surfaces a 503 without a 90s hang per request.
const reNavTimeout = 30 * time.Second

// NewBrowser starts a shared Chrome process and warms the playground page.
// Call Close when done.
//
// Container / proxy hints:
//   - CHROME_PATH: absolute path to chromium/chrome binary
//   - CHROMEDP_NO_SANDBOX=1: add --no-sandbox and --disable-dev-shm-usage
//   - CHROMEDP_ALLOW_IMAGES=1: re-enable image loading (default is off). Pictures
//     are unnecessary for this invisible hCaptcha widget, so images are blocked
//     by default to cut per-navigate RAM/bandwidth; re-enable only if a future
//     site change makes image decode required for token extraction.
//   - CHROME_PROXY / BrowserConfig.Proxy: Chrome --proxy-server (e.g. socks5://host:port)
//   - BrowserConfig.Playground: playground URL to mint tokens on (default is
//     minimax-m3; override when that model retires)
func NewBrowser(parent context.Context, cfg BrowserConfig) (*Browser, error) {
	cfg = cfg.withDefaults()
	allocOpts := ChromeAllocatorOptions()
	if path := os.Getenv("CHROME_PATH"); path != "" {
		allocOpts = append(allocOpts, chromedp.ExecPath(path))
	}
	if os.Getenv("CHROMEDP_NO_SANDBOX") == "1" {
		allocOpts = append(allocOpts,
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-dev-shm-usage", true),
		)
	}
	// Images are blocked by default (verified end-to-end: a token extracted with
	// imagesEnabled=false is accepted by the upstream predict API → HTTP 200).
	// CHROMEDP_ALLOW_IMAGES=1 opts back in for debugging unexpected hCaptcha change.
	if os.Getenv("CHROMEDP_ALLOW_IMAGES") != "1" {
		allocOpts = append(allocOpts,
			chromedp.Flag("blink-settings", "imagesEnabled=false"),
		)
	}
	if cfg.Proxy != "" {
		allocOpts = append(allocOpts, chromeProxyOpts(cfg.Proxy)...)
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(parent, allocOpts...)
	// Drop chromedp's noisy "unhandled … event" logs (e.g. TopLayerElementsUpdated).
	// They are CDP events the library does not model; they do not abort Run.
	browser, bCancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(quietChromedpErrorf))

	// Allocate Chrome on browserCtx with no canceling timeout (CommandContext
	// would kill the process if the first-Run context is canceled).
	if err := chromedp.Run(browser, chromedp.Navigate("about:blank")); err != nil {
		bCancel()
		allocCancel()
		return nil, fmt.Errorf("captcha browser alloc: %w", err)
	}

	// Use explicit playground URL from config (no auto-probe).
	pgURL := cfg.Playground

	b := &Browser{
		browser: browser,
		cancel:  allocCancel,
		bCancel: bCancel,
		pgURL:   pgURL,
	}

	// Warm playground once so Extract can skip Navigate in the steady state.
	if !cfg.NoWarm {
		warmCtx, warmCancel := context.WithTimeout(browser, 90*time.Second)
		defer warmCancel()
		if err := warmPlayground(warmCtx, pgURL); err != nil {
			b.Close()
			return nil, fmt.Errorf("captcha browser warm: %w", err)
		}
		b.warmed = true
		b.lastOK = time.Now()
	}
	return b, nil
}

// ProbeNav re-navigates the sticky tab to url and times the full page warm-up
// (assets blocked, widget mounted) — the metric playground selection ranks
// candidates by. On success the tab stays mounted on url, so the selection's
// winner Chrome can be handed to the pool with its first mint pre-paid.
func (b *Browser) ProbeNav(ctx context.Context, url string) (time.Duration, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, fmt.Errorf("captcha browser closed")
	}
	tctx, cancel := context.WithTimeout(b.browser, reNavTimeout)
	defer cancel()
	start := time.Now()
	if err := warmPlayground(tctx, url); err != nil {
		b.warmed = false
		return 0, err
	}
	b.pgURL = url
	b.warmed = true
	b.lastOK = time.Now()
	// Batch widgets don't survive a navigate — reset like the re-nav path does.
	b.sitekey = ""
	b.extraIDs = nil
	return time.Since(start), nil
}

// Extract returns a one-shot captcha token from the sticky playground tab.
// Concurrent callers are serialized (one tab); steady-state cost is execute({async:true}).
func (b *Browser) Extract(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return "", fmt.Errorf("captcha browser closed")
	}

	// Sticky execute is normally ~300ms; bound it tightly so a hung widget
	// fails fast and re-navigate can start (pool Take otherwise waits with
	// no tokens). 5s is enough for a healthy sticky path and avoids burning
	// most of captcha-wait (30s) before recovery begins.
	needNav := !b.warmed || time.Since(b.lastOK) > stickyMaxIdle
	if needNav {
		b.navCount.Add(1)
		token, err := b.runExtract(ctx, reNavTimeout, func(c context.Context) (string, error) {
			return navigateAndExecute(c, b.pgURL)
		})
		if err != nil {
			b.warmed = false
			return "", err
		}
		b.warmed = true
		b.lastOK = time.Now()
		return token, nil
	}

	b.stickyCount.Add(1)
	token, err := b.runExtract(ctx, 5*time.Second, executeOnly)
	if err == nil {
		b.lastOK = time.Now()
		return token, nil
	}
	// Page may have broken (navigation, bot wall, widget gone) — full recover.
	b.navCount.Add(1)
	token, navErr := b.runExtract(ctx, reNavTimeout, func(c context.Context) (string, error) {
		return navigateAndExecute(c, b.pgURL)
	})
	if navErr != nil {
		b.warmed = false
		return "", fmt.Errorf("sticky execute failed (%v); re-navigate failed: %w", err, navErr)
	}
	b.warmed = true
	b.lastOK = time.Now()
	return token, nil
}

func (b *Browser) runExtract(ctx context.Context, limit time.Duration, fn func(context.Context) (string, error)) (string, error) {
	runCtx, cancel := context.WithTimeout(b.browser, limit)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	return fn(runCtx)
}

// Close shuts down the shared Chrome process.
func (b *Browser) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	b.warmed = false
	if b.bCancel != nil {
		b.bCancel()
	}
	b.cancel()
}

// NavCount returns the number of re-navigates (slow path ~6-10s).
func (b *Browser) NavCount() uint64 {
	return b.navCount.Load()
}

// StickyCount returns the number of sticky executes (fast path ~300ms).
func (b *Browser) StickyCount() uint64 {
	return b.stickyCount.Load()
}

// quietChromedpErrorf suppresses known-benign CDP events chromedp has not
// wired into its DOM/page switch (logged as ERROR otherwise).
func quietChromedpErrorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if strings.HasPrefix(msg, "unhandled node event") || strings.HasPrefix(msg, "unhandled page event") {
		return
	}
	log.Printf("ERROR: "+format, args...)
}
