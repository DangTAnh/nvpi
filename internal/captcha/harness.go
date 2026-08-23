package captcha

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// Harness page minting: the RAM in a normal mint is a renderer running the
// whole NVIDIA Next.js app just to host invisible hCaptcha widgets. The
// harness navigates to build.nvidia.com/robots.txt (3.6KB text/plain, no
// scripts) — same origin as the playground, so hCaptcha sees the hostname the
// sitekey is bound to — then replaces the document with ~15 lines of HTML via
// document.write. The Next.js bundle never downloads; only hcaptcha's api.js
// runs.
//
// The sitekey is NOT hardcoded: FetchSitekeyHTTP scrapes it from the page's
// own JS chunks over plain HTTP (no renderer), so an NVIDIA key rotation is
// picked up on every spawn.

var (
	// chunkSrcRE collects the page's Next.js script chunks from raw HTML.
	chunkSrcRE = regexp.MustCompile(`src="(/_next/[^"]+\.js)"`)
	// sitekeyPairRE matches the bundle's captcha config object:
	// {default:"<uuid>",otp:"<uuid>"} (observed live 2026-08-23).
	sitekeyPairRE = regexp.MustCompile(`default:"([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})",otp:"[0-9a-f]{8}-[0-9a-f]{4}"`)
	// sitekeyAttrRE is the fallback shape: captchaSiteKey:"<uuid>".
	sitekeyAttrRE = regexp.MustCompile(`captchaSiteKey:"([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})"`)
)

// FetchSitekeyHTTP resolves the hCaptcha sitekey for build.nvidia.com without
// launching a browser: fetch the playground HTML, then its _next chunks, and
// pull the key from whichever chunk carries it. Bounded-concurrency so a fat
// chunk list cannot stall startup.
func FetchSitekeyHTTP(ctx context.Context, hc *http.Client, pageURL string) (string, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("harness sitekey page fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("harness sitekey page status=%d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	base, err := url.Parse(pageURL)
	if err != nil {
		return "", err
	}
	var srcs []string
	for _, m := range chunkSrcRE.FindAllSubmatch(body, -1) {
		u, err := base.Parse(string(m[1]))
		if err != nil {
			continue
		}
		srcs = append(srcs, u.String())
	}
	if len(srcs) == 0 {
		return "", fmt.Errorf("no _next chunks found in %s", pageURL)
	}

	type hit struct{ key string }
	found := make(chan hit, len(srcs))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, src := range srcs {
		wg.Add(1)
		go func(src string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
			if err != nil {
				return
			}
			r, err := hc.Do(req)
			if err != nil {
				return
			}
			defer r.Body.Close()
			if r.StatusCode != 200 {
				return
			}
			cb, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
			for _, re := range []*regexp.Regexp{sitekeyPairRE, sitekeyAttrRE} {
				if m := re.FindSubmatch(cb); m != nil {
					found <- hit{key: string(m[1])}
					return
				}
			}
		}(src)
	}
	wg.Wait()
	close(found)

	for h := range found {
		return h.key, nil
	}
	return "", fmt.Errorf("sitekey not found in %d chunks", len(srcs))
}

// harnessHTML is the entire replacement document: preconnects for hcaptcha's
// asset origins plus api.js and an onload stub. No NVIDIA code ships.
func harnessHTML() string {
	return `<!doctype html><html><head><meta charset="utf-8"><title>playground</title>` +
		`<link rel="preconnect" href="https://js.hcaptcha.com" crossorigin>` +
		`<link rel="preconnect" href="https://newassets.hcaptcha.com" crossorigin>` +
		`<script src="https://js.hcaptcha.com/1/api.js?onload=onHcReady&render=explicit&uj=false"></script>` +
		`</head><body><div id="h0" style="display:none"></div>` +
		`<script>window.onHcReady=function(){};</script></body></html>`
}

// warmHarness puts ctx's tab on the harness page for pageURL's origin:
// navigate robots.txt (same origin, tiny, zero scripts of its own), then
// document.write over it — window.location stays on build.nvidia.com with only
// hcaptcha JS. Error strings match isHardExtractFailure's expectations.
func warmHarness(ctx context.Context, pageURL string) error {
	if err := chromedp.Run(ctx,
		chromedp.Navigate(robotsTxtURL(pageURL)),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("chromedp navigate: %w", err)
	}
	writeJS := fmt.Sprintf(`document.open();document.write(%q);document.close();true`, harnessHTML())
	if err := chromedp.Run(ctx, chromedp.Evaluate(writeJS, nil)); err != nil {
		return fmt.Errorf("chromedp write: %w", err)
	}
	if err := chromedp.Run(ctx, waitHCaptchaReady()); err != nil {
		return err
	}
	return nil
}

// ExtractHarness mints up to n tokens on a harness page at the playground's
// origin: one headless shell warms the harness document, renders n invisible
// widgets with the sitekey, and batch-executes them. Closes its Chrome before
// returning.
func ExtractHarness(ctx context.Context, cfg BrowserConfig, playgroundURL, sitekey string, n int) ([]string, error) {
	if n < 1 {
		n = 1
	}
	cfg.NoWarm = true
	cfg.Harness = true
	cfg.HarnessSitekey = sitekey
	b, err := NewBrowser(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer b.Close()
	bctx := b.browser

	warmCtx, warmCancel := context.WithTimeout(bctx, 60*time.Second)
	defer warmCancel()
	if err := warmHarness(warmCtx, playgroundURL); err != nil {
		return nil, fmt.Errorf("harness warm: %w", err)
	}

	var ids []string
	render := jsInvoke(renderWidgetsJS, fmt.Sprintf("%q", sitekey), fmt.Sprint(n))
	if err := chromedp.Run(bctx, chromedp.Evaluate(render, &ids)); err != nil || len(ids) == 0 {
		return nil, fmt.Errorf("harness render widgets: %w", err)
	}
	fresh := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" && !strings.HasPrefix(id, "ERR:") {
			fresh = append(fresh, id)
		}
	}
	fresh = waitWidgetsMounted(bctx, fresh, 10*time.Second)
	if len(fresh) == 0 {
		return nil, fmt.Errorf("harness widgets never mounted")
	}
	return batchExecuteIDs(bctx, fresh)
}

// robotsTxtURL swaps any path on build.nvidia.com for its lightweight
// robots.txt, keeping scheme+host (and therefore the hCaptcha origin).
func robotsTxtURL(pageURL string) string {
	u, err := url.Parse(pageURL)
	if err != nil || u.Host == "" {
		return "https://build.nvidia.com/robots.txt"
	}
	return u.Scheme + "://" + u.Host + "/robots.txt"
}
