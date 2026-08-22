package captcha

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// Batch minting on one sticky playground tab. The NVIDIA page mounts a single
// invisible hCaptcha widget; hcaptcha.render() accepts additional invisible
// widgets on the same document, and each mints independently (~300ms after
// mount, all in parallel). Verified live 2026-08-22: 6/6 widgets minted
// 1.2–1.7s per round (round 1 ~5s while new widgets warm), tokens accepted by
// the upstream predict endpoint (HTTP 200). One Chrome borrow now yields up to
// n tokens instead of 1.
//
// Degradation is layered: if rendering extra widgets fails (sitekey not found,
// hcaptcha.render throws, page change), ExtractBatch falls back to sequential
// executes on the primary widget (1 token per execute, the pre-batch behavior),
// and if that fails entirely the caller sees an error like the single path.

// minTokenLen separates real hCaptcha tokens (~1.9KB) from short stub values
// getResponse() returns for widgets that have not completed (~15–26 chars).
const minTokenLen = 100

// batchStickyTimeout bounds one sticky batch execute. The single path uses 5s;
// a batch fires n executes in parallel plus a first-round widget mount, so a
// little more headroom avoids false timeouts without delaying recovery much.
const batchStickyTimeout = 10 * time.Second

// runExtractBatch mirrors Browser.runExtract for batch-shaped mints (Go
// methods cannot be generic): bounds the mint with its own timeout and cancels
// early when the caller's ctx dies.
func (b *Browser) runExtractBatch(ctx context.Context, limit time.Duration, fn func(context.Context) ([]string, error)) ([]string, error) {
	runCtx, cancel := context.WithTimeout(b.browser, limit)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	return fn(runCtx)
}

// ExtractBatch borrows nothing itself — the caller (BrowserGroup.ExtractBatch)
// already serialized via the group's free-list. It mints up to n tokens on the
// sticky tab: extra widgets are rendered once and reused while warm. Returns
// what succeeded; an error only when zero tokens were minted.
func (b *Browser) ExtractBatch(ctx context.Context, n int) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, fmt.Errorf("captcha browser closed")
	}
	if n < 1 {
		n = 1
	}

	needNav := !b.warmed || time.Since(b.lastOK) > stickyMaxIdle
	if needNav {
		b.navCount.Add(1)
		toks, err := b.runExtractBatch(ctx, reNavTimeout, func(c context.Context) ([]string, error) {
			return b.navigateAndExecuteBatch(c, n)
		})
		if err != nil || len(toks) == 0 {
			b.warmed = false
			if err == nil {
				err = fmt.Errorf("empty captcha token (batch after navigate)")
			}
			return toks, err
		}
		b.warmed = true
		b.lastOK = time.Now()
		return toks, nil
	}

	b.stickyCount.Add(1)
	toks, err := b.runExtractBatch(ctx, batchStickyTimeout, func(c context.Context) ([]string, error) {
		return b.executeBatch(c, n)
	})
	if err == nil && len(toks) > 0 {
		b.lastOK = time.Now()
		return toks, nil
	}

	// Sticky batch failed (widget died, render broke, page changed) — same
	// recovery as the single path: one full re-navigate before giving up.
	b.navCount.Add(1)
	toks, navErr := b.runExtractBatch(ctx, reNavTimeout, func(c context.Context) ([]string, error) {
		return b.navigateAndExecuteBatch(c, n)
	})
	if navErr != nil || len(toks) == 0 {
		b.warmed = false
		if navErr == nil {
			navErr = fmt.Errorf("empty captcha token (batch after navigate)")
		}
		if err != nil {
			return toks, fmt.Errorf("sticky batch failed (%v); re-navigate failed: %w", err, navErr)
		}
		return toks, navErr
	}
	b.warmed = true
	b.lastOK = time.Now()
	return toks, nil
}

// navigateAndExecuteBatch re-warms the playground and mints a batch. Extra
// widgets do not survive navigation, so the cache resets and re-provisions.
func (b *Browser) navigateAndExecuteBatch(ctx context.Context, n int) ([]string, error) {
	if err := warmPlayground(ctx, b.pgURL); err != nil {
		return nil, fmt.Errorf("chromedp navigate: %w", err)
	}
	b.sitekey, b.extraIDs = "", nil
	return b.executeBatch(ctx, n)
}

// executeBatch mints n tokens with the best mechanism available: parallel
// executes across primary + rendered widgets, or — when extra widgets are
// unavailable — sequential executes on the primary widget (legacy behavior).
func (b *Browser) executeBatch(ctx context.Context, n int) ([]string, error) {
	ids := b.ensureWidgets(ctx, n)
	if len(ids) < 2 {
		toks := make([]string, 0, n)
		for i := 0; i < n; i++ {
			tok, err := executeOnly(ctx)
			if err != nil {
				if len(toks) > 0 {
					return toks, nil // partial success beats a synthetic error
				}
				return nil, err
			}
			toks = append(toks, tok)
		}
		return toks, nil
	}
	return batchExecuteIDs(ctx, ids)
}

// ensureWidgets returns the widget ids to execute for a batch of n: the page's
// primary widget plus cached rendered widgets, topping up renders to reach n.
// Best-effort: any failure returns whatever exists (possibly just the primary).
func (b *Browser) ensureWidgets(ctx context.Context, n int) []string {
	var domIDs []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(listWidgetIDsJS, &domIDs)); err != nil || len(domIDs) == 0 {
		return nil
	}
	primary := domIDs[0]

	// Keep only cached extras still in the DOM (a re-navigate wipes them).
	alive := make(map[string]bool, len(domIDs))
	for _, id := range domIDs {
		alive[id] = true
	}
	kept := b.extraIDs[:0]
	for _, id := range b.extraIDs {
		if alive[id] {
			kept = append(kept, id)
		}
	}
	b.extraIDs = kept

	if need := n - 1 - len(b.extraIDs); need > 0 {
		if b.sitekey == "" {
			var sk string
			if err := chromedp.Run(ctx, chromedp.Evaluate(sitekeyJS, &sk)); err != nil || sk == "" {
				log.Printf("captcha batch: sitekey not found (%v); single-widget mode", err)
				return []string{primary}
			}
			b.sitekey = sk
		}
		var newIDs []string
		render := jsInvoke(renderWidgetsJS, fmt.Sprintf("%q", b.sitekey), strconv.Itoa(need))
		if err := chromedp.Run(ctx, chromedp.Evaluate(render, &newIDs)); err != nil || len(newIDs) == 0 {
			log.Printf("captcha batch: hcaptcha.render failed (%v); single-widget mode", err)
			return []string{primary}
		}
		fresh := make([]string, 0, len(newIDs))
		for _, id := range newIDs {
			if id != "" && !strings.HasPrefix(id, "ERR:") && id != primary {
				fresh = append(fresh, id)
			}
		}
		// Rendered widgets mount their frames asynchronously — executing too
		// early fails silently (verified live: immediate execute yields zero
		// tokens; a mount wait makes it deterministic).
		fresh = waitWidgetsMounted(ctx, fresh, 10*time.Second)
		if len(fresh) == 0 {
			log.Printf("captcha batch: rendered widgets never mounted; single-widget mode")
			return []string{primary}
		}
		b.extraIDs = append(b.extraIDs, fresh...)
	}
	return append([]string{primary}, b.extraIDs...)
}

// waitWidgetsMounted polls until every rendered widget id shows up in the DOM
// (hcaptcha.render returns the id before the iframe mounts) and returns those
// that made it before the timeout.
func waitWidgetsMounted(ctx context.Context, ids []string, timeout time.Duration) []string {
	deadline := time.Now().Add(timeout)
	for {
		var domIDs []string
		if err := chromedp.Run(ctx, chromedp.Evaluate(listWidgetIDsJS, &domIDs)); err == nil {
			dom := make(map[string]bool, len(domIDs))
			for _, id := range domIDs {
				dom[id] = true
			}
			mounted := make([]string, 0, len(ids))
			for _, id := range ids {
				if dom[id] {
					mounted = append(mounted, id)
				}
			}
			if len(mounted) == len(ids) || !time.Now().Before(deadline) {
				return mounted
			}
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		if err := chromedp.Sleep(100 * time.Millisecond).Do(ctx); err != nil {
			return nil
		}
	}
}

// batchExecuteIDs fires hcaptcha.execute on every widget id concurrently and
// polls each response independently. Returns distinct fresh tokens; error only
// when none minted.
func batchExecuteIDs(ctx context.Context, ids []string) ([]string, error) {
	var prevs []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(jsInvoke(readTokensJS, jsStringList(ids)), &prevs)); err != nil {
		return nil, fmt.Errorf("chromedp read prev: %w", err)
	}
	var fired bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(jsInvoke(fireAllJS, jsStringList(ids)), &fired)); err != nil || !fired {
		return nil, fmt.Errorf("chromedp execute: fired=%v err=%v", fired, err)
	}

	deadline := time.Now().Add(8 * time.Second)
	fresh := make([]bool, len(ids))
	toks := make([]string, len(ids))
	remaining := len(ids)
	for remaining > 0 && time.Now().Before(deadline) {
		var cur []string
		if err := chromedp.Run(ctx, chromedp.Evaluate(jsInvoke(readTokensJS, jsStringList(ids)), &cur)); err != nil {
			return nil, fmt.Errorf("chromedp poll: %w", err)
		}
		for i, t := range cur {
			if i < len(fresh) && !fresh[i] && len(t) >= minTokenLen && t != prevs[i] {
				fresh[i] = true
				toks[i] = t
				remaining--
			}
		}
		if remaining > 0 {
			if err := chromedp.Sleep(50 * time.Millisecond).Do(ctx); err != nil {
				return nil, fmt.Errorf("chromedp poll: %w", err)
			}
		}
	}

	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, t := range toks {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty captcha token (batch of %d widgets)", len(ids))
	}
	return out, nil
}

// jsInvoke calls a JS function expression with already-encoded arguments.
func jsInvoke(fn string, args ...string) string {
	return "(" + fn + ")(" + strings.Join(args, ",") + ")"
}

// jsStringList encodes ids as a JSON string array literal.
func jsStringList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return "[" + strings.Join(q, ",") + "]"
}

// sitekeyJS extracts the hCaptcha sitekey from any widget iframe URL. The
// checkbox iframe's fragment lacks it; the challenge iframe's carries
// `sitekey=...` (verified live). Fragment params, then query params.
const sitekeyJS = `(() => {
	const grab = (u) => {
		try {
			const s = String(u);
			const hash = s.split('#')[1] || '';
			for (const kv of hash.split('&')) {
				const i = kv.indexOf('=');
				if (i > 0 && kv.slice(0, i) === 'sitekey') return decodeURIComponent(kv.slice(i + 1));
			}
			return new URL(s).searchParams.get('sitekey') || '';
		} catch (e) { return ''; }
	};
	for (const f of document.querySelectorAll('iframe')) {
		const k = grab(f.src);
		if (k) return k;
	}
	return '';
})()`

// listWidgetIDsJS returns widget ids in document order; the primary (NVIDIA's)
// widget is first because rendered extras are appended later.
const listWidgetIDsJS = `(() => Array.from(document.querySelectorAll('[data-hcaptcha-widget-id]'))
	.map(e => e.getAttribute('data-hcaptcha-widget-id')))()`

// renderWidgetsJS renders count extra invisible widgets and returns their ids.
// A failed render is reported as "ERR:<message>" so Go can filter it out.
const renderWidgetsJS = `((sitekey, count) => {
	const out = [];
	for (let i = 0; i < count; i++) {
		try {
			const d = document.createElement('div');
			d.style.display = 'none';
			document.body.appendChild(d);
			out.push(String(hcaptcha.render(d, { sitekey: sitekey, size: 'invisible' })));
		} catch (e) { out.push('ERR:' + e.message); }
	}
	return out;
})`

// fireAllJS fires hcaptcha.execute(id, {async:true}) on every id. Fire-and-
// forget mirrors the single path: Go polls the responses.
const fireAllJS = `((ids) => {
	if (typeof hcaptcha === 'undefined') return false;
	ids.forEach(id => { try { hcaptcha.execute(String(id), { async: true }); } catch (e) {} });
	return true;
})`

// readTokensJS returns the current response for each widget id: getResponse
// first (works for rendered widgets), then the data-hcaptcha-response attribute.
const readTokensJS = `((ids) => ids.map(id => {
	let t = '';
	try {
		const r = typeof hcaptcha.getResponse === 'function' ? hcaptcha.getResponse(String(id)) : '';
		if (typeof r === 'string') t = r;
	} catch (e) {}
	if (!t) {
		const el = document.querySelector('[data-hcaptcha-widget-id="'+id+'"]');
		if (el) t = el.getAttribute('data-hcaptcha-response') || '';
	}
	return t;
}))`
